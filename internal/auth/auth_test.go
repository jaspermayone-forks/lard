package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAuthServer stands in for the authorization server's introspection endpoint.
func fakeAuthServer(t *testing.T, res map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/token/introspect" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		if r.PostForm.Get("token") != "good" {
			_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func oauthConfig(authServer string) Config {
	return Config{
		Mode:          ModeOAuth,
		AuthServerURL: authServer,
		PublicURL:     "https://lard.example.com",
	}
}

func do(h http.Handler, authz string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/memory/crush/profile.md", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := IdentityFrom(r.Context()); ok {
			w.Header().Set("X-Subject", id.Subject)
			w.Header().Set("X-Client", id.ClientID)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestOAuthAcceptsActiveToken(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"client_id": "https://app.example.com", "scope": "profile email",
		"aud": "https://lard.example.com",
	})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	w := do(h, "Bearer good")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Subject"); got != "https://auth.example.com/u/kieran" {
		t.Errorf("identity not propagated: %q", got)
	}
}

func TestOAuthRejectsInactiveToken(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{"active": true})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	w := do(h, "Bearer nope")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	// The challenge must point at the resource metadata, or an MCP client has
	// no way to discover where to get a token.
	if ch := w.Header().Get("WWW-Authenticate"); !strings.Contains(ch, "resource_metadata=") {
		t.Errorf("challenge missing resource_metadata: %q", ch)
	}
}

func TestOAuthRejectsMissingHeader(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{"active": true})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	if w := do(h, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// A token minted for a different app must not open lard. This is the confused
// deputy case: the provider happily issues tokens for every app the user
// authorizes, and only the audience distinguishes them.
func TestOAuthRejectsForeignClient(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"client_id": "https://someone-elses-app.example.com", "scope": "profile",
		"aud": "https://someone-elses-app.example.com",
	})
	w := do(Middleware(oauthConfig(srv.URL), okHandler()), "Bearer good")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for foreign client, got %d", w.Code)
	}
}

func TestOAuthRejectsWrongAudience(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"aud": "https://other.example.com",
	})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a token minted for another resource, got %d", w.Code)
	}
}

func TestOAuthAcceptsMatchingAudience(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"aud": "https://lard.example.com/", // trailing slash is normalized away
	})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusOK {
		t.Fatalf("want 200 for a token whose audience is this resource, got %d", w.Code)
	}
}

func TestOAuthAcceptsAudienceArrayContainingResource(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"aud": []string{"https://other.example.com", "https://lard.example.com"},
	})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusOK {
		t.Fatalf("want 200 when the aud array includes this resource, got %d", w.Code)
	}
}

func TestOAuthRejectsUnscopedToken(t *testing.T) {
	// A token with no aud names no resource, so it is no better than a
	// stranger's: it could have been minted for anything.
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
	})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a token with no audience, got %d", w.Code)
	}
}

func TestOAuthRejectsUnlistedUser(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/stranger",
		"client_id": "https://app.example.com", "scope": "profile",
		"aud": "https://lard.example.com",
	})
	cfg := oauthConfig(srv.URL)
	cfg.AllowedUsers = []string{"https://auth.example.com/u/kieran"}
	h := Middleware(cfg, okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestOAuthRejectsInsufficientScope(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{
		"active": true, "me": "https://auth.example.com/u/kieran",
		"client_id": "https://app.example.com", "scope": "profile",
		"aud": "https://lard.example.com",
	})
	cfg := oauthConfig(srv.URL)
	cfg.RequiredScopes = []string{"email"}
	h := Middleware(cfg, okHandler())
	w := do(h, "Bearer good")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if ch := w.Header().Get("WWW-Authenticate"); !strings.Contains(ch, "insufficient_scope") {
		t.Errorf("want insufficient_scope challenge, got %q", ch)
	}
}

// Discovery must work unauthenticated, otherwise a client cannot bootstrap.
func TestDiscoveryIsUnauthenticated(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{"active": true})
	cfg := oauthConfig(srv.URL)
	mux := http.NewServeMux()
	mux.Handle(PathProtectedResource, ProtectedResourceMetadata(cfg))
	mux.Handle(PathProtectedResource+"/", ProtectedResourceMetadata(cfg))
	mux.Handle(PathAuthServer, AuthServerMetadata(cfg))
	h := Middleware(cfg, mux)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathProtectedResource, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("resource metadata: want 200, got %d", w.Code)
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata not JSON: %v", err)
	}
	if doc.Resource != "https://lard.example.com" {
		t.Errorf("resource = %q", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != srv.URL {
		t.Errorf("authorization_servers = %v", doc.AuthorizationServers)
	}

	// RFC 9728 keeps the resource path as a suffix on the well-known URL,
	// and that suffixed form is what MCP clients request.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, PathProtectedResource+"/mcp", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("suffixed metadata: want 200, got %d", w2.Code)
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &doc); err != nil {
		t.Fatalf("suffixed metadata not JSON: %v", err)
	}
	if doc.Resource != "https://lard.example.com/mcp" {
		t.Errorf("suffixed resource = %q, want .../mcp", doc.Resource)
	}
}

// A 401 on /mcp must name the /mcp metadata document, not the base one, or the
// client will ask the provider for a token scoped to the wrong resource.
func TestChallengeOnMCPNamesMCPResource(t *testing.T) {
	srv := fakeAuthServer(t, map[string]any{"active": true})
	h := Middleware(oauthConfig(srv.URL), okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	want := `resource_metadata="https://lard.example.com` + PathProtectedResource + `/mcp"`
	if ch := w.Header().Get("WWW-Authenticate"); !strings.Contains(ch, want) {
		t.Errorf("challenge = %q, want it to contain %s", ch, want)
	}
}

// Redirecting instead of proxying keeps the issuer consistent with the URL the
// client fetched, which RFC 8414 requires clients to verify.
func TestAuthServerMetadataRedirects(t *testing.T) {
	cfg := oauthConfig("https://auth.example.com")
	w := httptest.NewRecorder()
	AuthServerMetadata(cfg)(w, httptest.NewRequest(http.MethodGet, PathAuthServer, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	want := "https://auth.example.com" + PathAuthServer
	if got := w.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

func TestDiscoveryHiddenWhenNotOAuthMode(t *testing.T) {
	cfg := Config{Mode: ModeNone}
	w := httptest.NewRecorder()
	ProtectedResourceMetadata(cfg)(w, httptest.NewRequest(http.MethodGet, PathProtectedResource, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestSharedSecretMode(t *testing.T) {
	h := Middleware(Config{Mode: ModeToken, Token: "s3cret"}, okHandler())
	if w := do(h, "Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("valid secret: want 200, got %d", w.Code)
	}
	if w := do(h, "Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad secret: want 401, got %d", w.Code)
	}
	if w := do(h, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no secret: want 401, got %d", w.Code)
	}
}

// An empty configured secret must never authenticate an empty request.
func TestSharedSecretModeRejectsEmptyConfig(t *testing.T) {
	h := Middleware(Config{Mode: ModeToken}, okHandler())
	if w := do(h, "Bearer "); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// Introspection failures must fail closed, not open.
func TestOAuthFailsClosedWhenProviderUnreachable(t *testing.T) {
	cfg := oauthConfig("http://127.0.0.1:1")
	h := Middleware(cfg, okHandler())
	if w := do(h, "Bearer good"); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestValidateWarnsAboutMissingAuthServer(t *testing.T) {
	warns := Config{Mode: ModeOAuth, PublicURL: "https://l"}.Validate()
	if len(warns) != 1 || !strings.Contains(warns[0], "authorization server") {
		t.Fatalf("want an authorization server warning, got %v", warns)
	}
	cfg := Config{Mode: ModeOAuth, AuthServerURL: "https://i", PublicURL: "https://l"}
	if warns := cfg.Validate(); len(warns) != 0 {
		t.Fatalf("want no warnings, got %v", warns)
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	h := Middleware(Config{Mode: ModeToken, Token: "s3cret"}, okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}
