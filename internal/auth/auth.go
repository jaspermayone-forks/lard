// Package auth gates lard's HTTP and MCP surfaces. Two modes:
//
//   - bearer: validate tokens against indiko's introspection endpoint
//     (RFC 7662), and publish OAuth metadata so MCP clients can discover
//     indiko as the authorization server.
//   - token: a single shared secret (LARD_TOKEN) for the collector path
//     when the full OAuth dance is overkill (e.g. a homelab cron).
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Mode selects the auth strategy.
type Mode string

const (
	ModeNone   Mode = "none"
	ModeBearer Mode = "bearer" // indiko OAuth
	ModeToken  Mode = "token"  // shared secret
)

// Config holds auth settings.
type Config struct {
	Mode Mode
	// Token is the shared secret for ModeToken.
	Token string
	// IndikoURL is the indiko base URL (https://indiko.dunkirk.sh) for ModeBearer.
	IndikoURL string
	// PublicURL is lard's own externally reachable base URL; used to build
	// OAuth metadata pointing at indiko.
	PublicURL string
}

// Middleware enforces the configured mode on all requests except
// /healthz and the OAuth discovery documents.
func Middleware(cfg Config, next http.Handler) http.Handler {
	v := &verifier{cfg: cfg, cache: map[string]cacheEntry{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || strings.HasPrefix(p, "/.well-known/") {
			next.ServeHTTP(w, r)
			return
		}
		switch cfg.Mode {
		case ModeNone:
			next.ServeHTTP(w, r)
		case ModeToken:
			if !checkToken(cfg.Token, r) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lard"`)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		case ModeBearer:
			if !v.checkBearer(r) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="lard", authorization_uri="%s/.well-known/oauth-authorization-server"`, strings.TrimRight(cfg.IndikoURL, "/")))
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
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
	active  bool
	expires time.Time
}

type verifier struct {
	cfg   Config
	mu    sync.Mutex
	cache map[string]cacheEntry
}

func (v *verifier) checkBearer(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return false
	}
	v.mu.Lock()
	if e, ok := v.cache[tok]; ok && time.Now().Before(e.expires) {
		v.mu.Unlock()
		return e.active
	}
	v.mu.Unlock()

	active, ttl := v.introspect(r.Context(), tok)
	v.mu.Lock()
	v.cache[tok] = cacheEntry{active: active, expires: time.Now().Add(ttl)}
	v.mu.Unlock()
	return active
}

// introspect asks indiko whether the token is live. Fail closed.
func (v *verifier) introspect(ctx context.Context, tok string) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(v.cfg.IndikoURL, "/")+"/auth/token/introspect",
		strings.NewReader("token="+tok))
	if err != nil {
		return false, 0
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0
	}
	var body struct {
		Active bool `json:"active"`
		Exp    int64 `json:"exp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, 0
	}
	ttl := 5 * time.Minute
	if body.Exp > 0 {
		if d := time.Until(time.Unix(body.Exp, 0)); d < ttl {
			ttl = d
		}
	}
	return body.Active, ttl
}

// WellKnownOAuthMetadata proxies indiko's authorization-server metadata so
// MCP clients discovering OAuth from lard's base URL land on indiko.
func WellKnownOAuthMetadata(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Mode != ModeBearer || cfg.IndikoURL == "" {
			http.NotFound(w, r)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
			strings.TrimRight(cfg.IndikoURL, "/")+"/.well-known/oauth-authorization-server", nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 64<<10)
		n, _ := resp.Body.Read(buf)
		w.Write(buf[:n])
	}
}
