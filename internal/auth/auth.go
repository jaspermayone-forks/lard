// Package auth gates lard's HTTP and MCP surfaces. Two modes:
//
//   - oauth: lard is an OAuth 2.1 protected resource. Access tokens are
//     validated against the authorization server's introspection endpoint
//     (RFC 7662), and lard publishes protected-resource metadata (RFC 9728)
//     so MCP clients can discover the authorization server.
//   - token: a single shared secret (LARD_TOKEN) for the collector path when
//     the full OAuth dance is overkill (e.g. a homelab cron).
//
// The authorization server issues tokens for every app the user authorizes,
// so lard must check that a token was actually minted for lard. Without that
// check any app the user has ever signed into could read their whole memory
// (the "confused deputy" problem). Set an audience or user allowlist to
// close it.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Mode selects the auth strategy.
type Mode string

const (
	ModeNone  Mode = "none"
	ModeOAuth Mode = "oauth" // OAuth 2.1 protected resource
	ModeToken Mode = "token" // shared secret
)

// Discovery paths lard serves. The authorization-server path exists only to
// point 2025-03-26-era MCP clients at the authorization server; newer clients
// protected-resource document.
const (
	PathProtectedResource = "/.well-known/oauth-protected-resource"
	PathAuthServer        = "/.well-known/oauth-authorization-server"
	// PathCollector is the unauthenticated prefix serving the collector OAuth
	// registration and, for confidential clients, the code exchange.
	PathCollector = "/auth/collector"
)

// Config holds auth settings.
type Config struct {
	Mode Mode
	// Token is the shared secret for ModeToken.
	Token string
	// AuthServerURL is the authorization server's base URL for ModeOAuth.
	AuthServerURL string
	// PublicURL is lard's own externally reachable base URL. It is the
	// resource identifier in the protected-resource metadata, so clients know
	// which resource they are asking the authorization server for a token for.
	PublicURL string
	// AllowedClientIDs limits which OAuth clients may call lard, matched
	// against the client_id on the introspected token. Empty means any client
	// the user has authorized is accepted.
	AllowedClientIDs []string
	// AllowedUsers limits which identities may call lard, matched against
	// the token's "me" URL. Empty means any user is accepted.
	AllowedUsers []string
	// RequiredScopes are scopes every token must carry. Empty means none.
	RequiredScopes []string
	// CollectorClientID is the OAuth client this server tells collectors to
	// use. It is trusted implicitly: publishing an identity and then rejecting
	// it would be a contradiction, and forcing the operator to repeat it in
	// AllowedClientIDs is a step that only ever gets forgotten.
	CollectorClientID string
}

// clientAllowlist is the set of client ids accepted, including the collector
// registration this server hands out.
func (c Config) clientAllowlist() []string {
	if c.CollectorClientID == "" {
		return c.AllowedClientIDs
	}
	if len(c.AllowedClientIDs) == 0 {
		// An explicit collector id is itself a restriction, so honor it as the
		// whole allowlist rather than treating "no list" as "allow anything".
		return []string{c.CollectorClientID}
	}
	return append(append([]string{}, c.AllowedClientIDs...), c.CollectorClientID)
}

// Validate reports configuration problems worth logging at boot. OAuth mode
// with no allowlist works, but it trusts every app the user has authorized.
func (c Config) Validate() []string {
	var warns []string
	if c.Mode != ModeOAuth {
		return nil
	}
	if c.AuthServerURL == "" {
		warns = append(warns, "oauth auth has no authorization server URL; every request will be rejected")
	}
	if c.PublicURL == "" {
		warns = append(warns, "oauth auth has no public_url; OAuth discovery metadata will be incomplete")
	}
	if len(c.clientAllowlist()) == 0 && len(c.AllowedUsers) == 0 {
		warns = append(warns, "oauth auth has no audience restriction: any app the user authorized with the same provider can read all memory (set allowed_client_ids or allowed_users)")
	}
	return warns
}

// Identity is who the caller turned out to be. It rides on the request
// context so handlers can attribute writes later.
type Identity struct {
	Subject  string // authorization server "me" URL
	ClientID string
	Scopes   []string
}

type identityKey struct{}

// IdentityFrom returns the authenticated caller, if any.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Middleware enforces the configured mode on all requests except /healthz and
// the OAuth discovery documents, which must stay reachable so an unauthorized
// client can learn where to get a token.
func Middleware(cfg Config, next http.Handler) http.Handler {
	v := &verifier{cfg: cfg, cache: map[string]cacheEntry{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Discovery and the collector registration must be reachable without
		// credentials: they are how a caller learns to get credentials.
		if p == "/healthz" || strings.HasPrefix(p, "/.well-known/") || strings.HasPrefix(p, PathCollector) {
			next.ServeHTTP(w, r)
			return
		}
		switch cfg.Mode {
		case ModeNone:
			next.ServeHTTP(w, r)
		case ModeToken:
			if !checkToken(cfg.Token, r) {
				writeChallenge(w, r, cfg, http.StatusUnauthorized, "invalid_token", "invalid or missing token")
				return
			}
			next.ServeHTTP(w, r)
		case ModeOAuth:
			id, status, code, desc := v.authorize(r)
			if status != 0 {
				writeChallenge(w, r, cfg, status, code, desc)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
		default:
			writeChallenge(w, r, cfg, http.StatusInternalServerError, "server_error", "unknown auth mode")
		}
	})
}

// writeChallenge emits an RFC 6750 challenge. The resource_metadata parameter
// (RFC 9728) is what lets an MCP client bootstrap the OAuth flow from a bare
// 401, without knowing anything about the authorization server up front. It names the metadata
// document for the exact path being refused, so a client hitting /mcp is sent
// to the /mcp-suffixed document.
func writeChallenge(w http.ResponseWriter, r *http.Request, cfg Config, status int, code, desc string) {
	params := []string{`realm="lard"`}
	if code != "" {
		params = append(params, fmt.Sprintf(`error=%q`, code))
	}
	if desc != "" {
		params = append(params, fmt.Sprintf(`error_description=%q`, desc))
	}
	base := strings.TrimRight(cfg.PublicURL, "/")
	if base == "" {
		base = requestBaseURL(r)
	}
	if base != "" {
		params = append(params, fmt.Sprintf(`resource_metadata=%q`, base+PathProtectedResource+resourceSuffix(r.URL.Path)))
	}
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// resourceSuffix maps a refused request path to the resource it belongs to.
// Only /mcp is a distinct resource; the REST surface is the base resource, so
// everything else gets an empty suffix rather than a per-path document.
func resourceSuffix(path string) string {
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		return "/mcp"
	}
	return ""
}

func checkToken(want string, r *http.Request) bool {
	if want == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type cacheEntry struct {
	result  introspection
	expires time.Time
}

type verifier struct {
	cfg   Config
	mu    sync.Mutex
	cache map[string]cacheEntry
}

// introspection is the subset of RFC 7662 fields lard acts on.
type introspection struct {
	Active   bool   `json:"active"`
	Me       string `json:"me"`
	Subject  string `json:"sub"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
	Audience string `json:"aud"`
	Exp      int64  `json:"exp"`
}

func (i introspection) subject() string {
	if i.Me != "" {
		return i.Me
	}
	return i.Subject
}

// authorize validates the access token and the claims on it. A zero status
// means the request may proceed; otherwise status/code/desc describe the
// refusal (401 for a bad token, 403 for a token that is valid but not for us).
func (v *verifier) authorize(r *http.Request) (id Identity, status int, code, desc string) {
	header := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if tok == "" || tok == header {
		return id, http.StatusUnauthorized, "invalid_request", "missing access token"
	}
	res, ok := v.introspectCached(r.Context(), tok)
	if !ok || !res.Active {
		return id, http.StatusUnauthorized, "invalid_token", "token is not active"
	}
	scopes := strings.Fields(res.Scope)
	// Denials are logged with the claims that failed. A 403 here is almost
	// always an allowlist typo, and the operator cannot see the token's real
	// client_id or "me" value any other way.
	if !allowed(v.cfg.clientAllowlist(), res.ClientID) {
		slog.Warn("auth: client not allowed",
			"token_client_id", res.ClientID, "allowed", v.cfg.clientAllowlist())
		return id, http.StatusForbidden, "invalid_token", "token was not issued for this resource"
	}
	if !allowed(v.cfg.AllowedUsers, res.subject()) {
		slog.Warn("auth: user not allowed",
			"token_user", res.subject(), "allowed", v.cfg.AllowedUsers)
		return id, http.StatusForbidden, "invalid_token", "user is not permitted"
	}
	for _, want := range v.cfg.RequiredScopes {
		if !hasScope(scopes, want) {
			slog.Warn("auth: scope missing", "want", want, "token_scopes", scopes)
			return id, http.StatusForbidden, "insufficient_scope", "missing scope " + want
		}
	}
	return Identity{Subject: res.subject(), ClientID: res.ClientID, Scopes: scopes}, 0, "", ""
}

// allowed reports whether v is in the list, treating an empty list as
// unrestricted. Client IDs and identity URLs are compared with trailing
// slashes normalized away, since providers and clients disagree about them.
func allowed(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	got := strings.TrimRight(strings.ToLower(strings.TrimSpace(v)), "/")
	for _, want := range list {
		if strings.TrimRight(strings.ToLower(strings.TrimSpace(want)), "/") == got {
			return true
		}
	}
	return false
}

func hasScope(scopes []string, want string) bool {
	return slices.Contains(scopes, want)
}

// introspectCached memoizes introspection results, keyed by a token digest so
// the raw secret is not a map key. Negative results are cached briefly too, so
// a client retrying with a dead token cannot hammer the provider.
func (v *verifier) introspectCached(ctx context.Context, tok string) (introspection, bool) {
	sum := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	if e, ok := v.cache[key]; ok && time.Now().Before(e.expires) {
		v.mu.Unlock()
		return e.result, true
	}
	v.mu.Unlock()

	res, ok := v.introspect(ctx, tok)
	if !ok {
		return introspection{}, false
	}
	ttl := 5 * time.Minute
	if !res.Active {
		ttl = 30 * time.Second
	} else if res.Exp > 0 {
		if d := time.Until(time.Unix(res.Exp, 0)); d < ttl {
			ttl = d
		}
	}
	if ttl > 0 {
		v.mu.Lock()
		v.cache[key] = cacheEntry{result: res, expires: time.Now().Add(ttl)}
		v.mu.Unlock()
	}
	return res, true
}

// introspect asks the authorization server whether the token is live. Fail
// closed.
func (v *verifier) introspect(ctx context.Context, tok string) (introspection, bool) {
	if v.cfg.AuthServerURL == "" {
		return introspection{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	form := url.Values{"token": {tok}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(v.cfg.AuthServerURL, "/")+"/auth/token/introspect",
		strings.NewReader(form.Encode()))
	if err != nil {
		return introspection{}, false
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("auth: introspection failed", "error", err)
		return introspection{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("auth: introspection rejected", "status", resp.StatusCode)
		return introspection{}, false
	}
	var body introspection
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		slog.Warn("auth: introspection response unreadable", "error", err)
		return introspection{}, false
	}
	return body, true
}

// ProtectedResourceMetadata serves the RFC 9728 document describing lard as a
// protected resource and naming its authorization server. This is the
// discovery entry point for MCP clients: they read it after a 401 and know
// where to send the user.
//
// RFC 9728 locates the document by inserting the well-known segment after the
// host, keeping the resource's own path as a suffix. So the resource
// https://lard.example/mcp is described at
// https://lard.example/.well-known/oauth-protected-resource/mcp. Clients ask
// for the suffixed form, so lard serves the whole subtree and echoes back
// whichever resource was requested.
func ProtectedResourceMetadata(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Mode != ModeOAuth || cfg.AuthServerURL == "" {
			http.NotFound(w, r)
			return
		}
		base := strings.TrimRight(cfg.PublicURL, "/")
		if base == "" {
			base = requestBaseURL(r)
		}
		suffix := strings.TrimPrefix(r.URL.Path, PathProtectedResource)
		doc := map[string]any{
			"resource":                 base + suffix,
			"authorization_servers":    []string{strings.TrimRight(cfg.AuthServerURL, "/")},
			"bearer_methods_supported": []string{"header"},
			"resource_documentation":   "https://github.com/taciturnaxolotl/lard",
		}
		if len(cfg.RequiredScopes) > 0 {
			doc["scopes_supported"] = cfg.RequiredScopes
		}
		w.Header().Set("content-type", "application/json")
		w.Header().Set("cache-control", "public, max-age=3600")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// AuthServerMetadata redirects to the authorization server's metadata.
// Proxying the body would be simpler but breaks clients: RFC 8414 makes them
// check that the issuer in the document matches where they fetched it from,
// and the AS's issuer is its own origin, not lard. A redirect keeps that
// invariant.
func AuthServerMetadata(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Mode != ModeOAuth || cfg.AuthServerURL == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, strings.TrimRight(cfg.AuthServerURL, "/")+PathAuthServer, http.StatusFound)
	}
}

// requestBaseURL reconstructs the externally visible base URL, honoring the
// reverse-proxy headers lard is likely to sit behind.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return scheme + "://" + host
}
