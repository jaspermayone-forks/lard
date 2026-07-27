package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterNotFoundWhenUnconfigured(t *testing.T) {
	w := httptest.NewRecorder()
	New(Config{}, DeviceConfig{}, "/auth/collector").Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRegisterPublishesIdentity(t *testing.T) {
	h := New(Config{ClientID: "ikc_abc", ClientSecret: "iks_xyz", TokenEndpoint: "https://as/token"}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var reg Registration
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.ClientID != "ikc_abc" {
		t.Errorf("clientId = %q", reg.ClientID)
	}
	if !reg.ServerExchange {
		t.Error("a confidential client must ask for a server-side exchange")
	}
	if len(reg.RedirectURIs) == 0 {
		t.Error("no redirect URIs published")
	}
	// The secret must never appear in the published document.
	if strings.Contains(w.Body.String(), "iks_xyz") {
		t.Fatal("client secret leaked into the registration document")
	}
}

func TestPublicClientDoesNotRequestServerExchange(t *testing.T) {
	h := New(Config{ClientID: "https://app.example.com/"}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	var reg Registration
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if reg.ServerExchange {
		t.Fatal("a public client should exchange its own code")
	}
}

// The exchange endpoint holds the client secret, so it must only ever redirect
// to a callback it published. Otherwise a stolen code plus an attacker-chosen
// redirect turns the server's credentials into an exchange oracle.
func TestValidateRedirectRejectsForeignTargets(t *testing.T) {
	h := New(Config{ClientID: "ikc_abc", ClientSecret: "s", Ports: []int{40714}}, DeviceConfig{}, "/auth/collector")
	bad := []string{
		"",
		"https://evil.example.com/callback",
		"http://localhost:9999/callback",
		"http://evil.example.com:40714/callback",
		"http://localhost/callback",
	}
	for _, r := range bad {
		if err := h.validateRedirect(r); err == nil {
			t.Errorf("validateRedirect(%q) = nil, want an error", r)
		}
	}
	good := []string{
		"http://localhost:40714/callback",
		"http://127.0.0.1:40714/callback",
	}
	for _, r := range good {
		if err := h.validateRedirect(r); err != nil {
			t.Errorf("validateRedirect(%q) = %v, want nil", r, err)
		}
	}
}

func TestExchangeRejectsMissingFields(t *testing.T) {
	h := New(Config{ClientID: "ikc_abc", ClientSecret: "s", TokenEndpoint: "https://as/token"}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"code":"abc"}`) // no verifier
	h.Exchange(w, httptest.NewRequest(http.MethodPost, "/auth/collector/exchange", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestExchangeDisabledForPublicClient(t *testing.T) {
	h := New(Config{ClientID: "https://app.example.com/"}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Exchange(w, httptest.NewRequest(http.MethodPost, "/auth/collector/exchange",
		strings.NewReader(`{"code":"a","codeVerifier":"b","redirectUri":"http://localhost:40714/callback"}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

// The exchange must send the secret to the authorization server, and pass the
// collector's PKCE verifier through unchanged.
func TestExchangeSendsSecretAndVerifier(t *testing.T) {
	var got map[string]string
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = map[string]string{}
		for k := range r.PostForm {
			got[k] = r.PostForm.Get(k)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3600}`))
	}))
	defer as.Close()

	h := New(Config{ClientID: "ikc_abc", ClientSecret: "iks_xyz", TokenEndpoint: as.URL, Ports: []int{40714}}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Exchange(w, httptest.NewRequest(http.MethodPost, "/auth/collector/exchange",
		strings.NewReader(`{"code":"CODE","codeVerifier":"VERIFIER","redirectUri":"http://localhost:40714/callback"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got["client_secret"] != "iks_xyz" {
		t.Errorf("client_secret = %q", got["client_secret"])
	}
	if got["code_verifier"] != "VERIFIER" {
		t.Errorf("code_verifier = %q", got["code_verifier"])
	}
	if got["client_id"] != "ikc_abc" {
		t.Errorf("client_id = %q", got["client_id"])
	}
	var out TokenResponse
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.AccessToken != "AT" || out.RefreshToken != "RT" {
		t.Errorf("token = %+v", out)
	}
	if out.Expiry.IsZero() {
		t.Error("expiry not derived from expires_in")
	}
}

// An authorization-server error must reach the user as a message, not a
// generic failure.
func TestExchangeSurfacesUpstreamError(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Authorization code not found"}`))
	}))
	defer as.Close()

	h := New(Config{ClientID: "ikc_abc", ClientSecret: "s", TokenEndpoint: as.URL, Ports: []int{40714}}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Exchange(w, httptest.NewRequest(http.MethodPost, "/auth/collector/exchange",
		strings.NewReader(`{"code":"bad","codeVerifier":"v","redirectUri":"http://localhost:40714/callback"}`)))
	if !strings.Contains(w.Body.String(), "Authorization code not found") {
		t.Fatalf("upstream error not surfaced: %s", w.Body.String())
	}
}

func TestRefreshUsesServerCredentials(t *testing.T) {
	var got map[string]string
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = map[string]string{}
		for k := range r.PostForm {
			got[k] = r.PostForm.Get(k)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"access_token":"AT2","expires_in":3600}`))
	}))
	defer as.Close()

	h := New(Config{ClientID: "ikc_abc", ClientSecret: "iks_xyz", TokenEndpoint: as.URL}, DeviceConfig{}, "/auth/collector")
	w := httptest.NewRecorder()
	h.Refresh(w, httptest.NewRequest(http.MethodPost, "/auth/collector/refresh",
		strings.NewReader(`{"refreshToken":"RT"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got["grant_type"] != "refresh_token" || got["client_secret"] != "iks_xyz" {
		t.Errorf("form = %v", got)
	}
}
