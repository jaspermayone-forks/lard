package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func deviceHandler(t *testing.T, tokenEndpoint string) *Handler {
	t.Helper()
	return New(
		Config{ClientID: "ikc_abc", ClientSecret: "iks_xyz", TokenEndpoint: tokenEndpoint},
		DeviceConfig{PublicURL: "https://lard.example.com", AuthorizationEndpoint: "https://as.example.com/auth/authorize"},
		"/auth/collector",
	)
}

func startSession(t *testing.T, h *Handler) deviceStartResponse {
	t.Helper()
	w := httptest.NewRecorder()
	h.StartDevice(w, httptest.NewRequest(http.MethodPost, "/auth/collector/device", strings.NewReader("{}")))
	if w.Code != http.StatusOK {
		t.Fatalf("StartDevice: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out deviceStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeviceUnavailableWithoutPublicURL(t *testing.T) {
	h := New(Config{ClientID: "x"}, DeviceConfig{AuthorizationEndpoint: "https://as/a"}, "/auth/collector")
	if h.DeviceAvailable() {
		t.Fatal("brokered login needs a public URL to receive the redirect")
	}
	w := httptest.NewRecorder()
	h.StartDevice(w, httptest.NewRequest(http.MethodPost, "/auth/collector/device", strings.NewReader("{}")))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestStartDeviceReturnsCodesAndURLs(t *testing.T) {
	out := startSession(t, deviceHandler(t, "https://as/token"))
	if out.DeviceCode == "" || out.UserCode == "" {
		t.Fatalf("missing codes: %+v", out)
	}
	// The verification URL must be on the server, not on the collector: that
	// is what makes it reachable from another machine.
	if !strings.HasPrefix(out.VerificationURI, "https://lard.example.com/auth/collector/device/verify") {
		t.Errorf("verificationUri = %q", out.VerificationURI)
	}
	if !strings.Contains(out.VerificationURIComplete, url.QueryEscape(out.UserCode)) {
		t.Errorf("complete URI should carry the code: %q", out.VerificationURIComplete)
	}
	if out.Interval <= 0 || out.ExpiresIn <= 0 {
		t.Errorf("interval/expiry not set: %+v", out)
	}
}

// The user code is typed by a human, so it must avoid glyphs that misread.
func TestUserCodeAvoidsAmbiguousCharacters(t *testing.T) {
	for range 200 {
		code, err := randomUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("unexpected shape: %q", code)
		}
		if strings.ContainsAny(code, "OIL01258BSZ") {
			t.Fatalf("ambiguous character in %q", code)
		}
	}
}

func TestNormalizeUserCodeIsForgiving(t *testing.T) {
	want := "ACDE-FGHJ"
	for _, in := range []string{"ACDE-FGHJ", "acde-fghj", "ACDEFGHJ", " acdefghj ", "acde fghj"} {
		if got := NormalizeUserCode(in); got != want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// Visiting the verification URL must send the user to the authorization server
// with PKCE and a redirect back to this server.
func TestVerifyRedirectsToAuthorizationServer(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	out := startSession(t, h)

	w := httptest.NewRecorder()
	h.Verify(w, httptest.NewRequest(http.MethodGet,
		"/auth/collector/device/verify?user_code="+url.QueryEscape(out.UserCode), nil))
	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("client_id") != "ikc_abc" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Error("PKCE challenge missing")
	}
	if q.Get("redirect_uri") != "https://lard.example.com/auth/collector/device/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Error("state missing; the callback could not find its session")
	}
}

func TestVerifyRejectsUnknownCode(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	w := httptest.NewRecorder()
	h.Verify(w, httptest.NewRequest(http.MethodGet, "/auth/collector/device/verify?user_code=ZZZZ-ZZZZ", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestVerifyShowsFormWithoutCode(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	w := httptest.NewRecorder()
	h.Verify(w, httptest.NewRequest(http.MethodGet, "/auth/collector/device/verify", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("want a code entry form, got %d: %s", w.Code, w.Body.String())
	}
}

// The full round trip: start, verify, callback, poll.
func TestDeviceFlowEndToEnd(t *testing.T) {
	var gotForm url.Values
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3600}`))
	}))
	defer as.Close()

	h := deviceHandler(t, as.URL)
	out := startSession(t, h)

	// Grab the state the way the authorization server would.
	vw := httptest.NewRecorder()
	h.Verify(vw, httptest.NewRequest(http.MethodGet,
		"/auth/collector/device/verify?user_code="+url.QueryEscape(out.UserCode), nil))
	loc, _ := url.Parse(vw.Header().Get("Location"))
	state := loc.Query().Get("state")

	// Before authorizing, polling reports pending rather than failing.
	if _, code := h.devices.poll(out.DeviceCode); code != ErrAuthorizationPending {
		t.Fatalf("want authorization_pending, got %q", code)
	}

	cw := httptest.NewRecorder()
	h.Callback(cw, httptest.NewRequest(http.MethodGet,
		"/auth/collector/device/callback?code=CODE&state="+url.QueryEscape(state), nil))
	if cw.Code != http.StatusOK {
		t.Fatalf("callback: want 200, got %d: %s", cw.Code, cw.Body.String())
	}
	// The secret must have been used server-side.
	if gotForm.Get("client_secret") != "iks_xyz" {
		t.Errorf("client_secret not sent: %v", gotForm)
	}
	if gotForm.Get("code_verifier") == "" {
		t.Error("code_verifier not sent")
	}

	// Now polling yields the token. The floor forces a small wait.
	time.Sleep(devicePollFloor + 50*time.Millisecond)
	tok, code := h.devices.poll(out.DeviceCode)
	if code != "" {
		t.Fatalf("poll: unexpected status %q", code)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Fatalf("token = %+v", tok)
	}

	// Single use: the code must not keep yielding a token.
	time.Sleep(devicePollFloor + 50*time.Millisecond)
	if _, code := h.devices.poll(out.DeviceCode); code != ErrInvalidGrant {
		t.Fatalf("device code was reusable; got %q", code)
	}
}

// A forged or stale state must not complete any session.
func TestCallbackRejectsUnknownState(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	w := httptest.NewRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet,
		"/auth/collector/device/callback?code=x&state=forged", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// A declined authorization must end the wait rather than leave the client
// polling until timeout.
func TestCallbackDenialIsReportedToPoller(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	out := startSession(t, h)
	sess, ok := h.devices.byUserCode(out.UserCode)
	if !ok {
		t.Fatal("session missing")
	}
	w := httptest.NewRecorder()
	h.Callback(w, httptest.NewRequest(http.MethodGet,
		"/auth/collector/device/callback?error=access_denied&state="+url.QueryEscape(sess.State), nil))
	time.Sleep(devicePollFloor + 50*time.Millisecond)
	if _, code := h.devices.poll(out.DeviceCode); code != ErrAccessDenied {
		t.Fatalf("want access_denied, got %q", code)
	}
}

func TestPollRateLimits(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	out := startSession(t, h)
	if _, code := h.devices.poll(out.DeviceCode); code != ErrAuthorizationPending {
		t.Fatalf("first poll: %q", code)
	}
	// Immediately again: too fast.
	if _, code := h.devices.poll(out.DeviceCode); code != ErrSlowDown {
		t.Fatalf("want slow_down, got %q", code)
	}
}

func TestPollUnknownDeviceCode(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	if _, code := h.devices.poll("nope"); code != ErrInvalidGrant {
		t.Fatalf("want invalid_grant, got %q", code)
	}
}

func TestExpiredSessionIsRejectedAndForgotten(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	out := startSession(t, h)
	sess, _ := h.devices.byUserCode(out.UserCode)
	sess.Expires = time.Now().Add(-time.Second)

	if _, code := h.devices.poll(out.DeviceCode); code != ErrExpiredToken {
		t.Fatalf("want expired_token, got %q", code)
	}
	if _, ok := h.devices.byUserCode(out.UserCode); ok {
		t.Error("expired session should be unreachable")
	}
}

// Pending and slow_down are progress, so they must not look like HTTP failures
// to a polling client.
func TestPollEndpointStatusCodes(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	out := startSession(t, h)
	body := `{"deviceCode":"` + out.DeviceCode + `"}`

	w := httptest.NewRecorder()
	h.PollDevice(w, httptest.NewRequest(http.MethodPost, "/auth/collector/device/token", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("pending should be 200, got %d", w.Code)
	}
	var out1 map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &out1)
	if out1["error"] != ErrAuthorizationPending {
		t.Errorf("error = %q", out1["error"])
	}

	w2 := httptest.NewRecorder()
	h.PollDevice(w2, httptest.NewRequest(http.MethodPost, "/auth/collector/device/token",
		strings.NewReader(`{"deviceCode":"bogus"}`)))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("unknown code should be 400, got %d", w2.Code)
	}
}

// The user code is what a human might read aloud, so codes must not collide
// across concurrent logins.
func TestConcurrentSessionsGetDistinctCodes(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	seen := map[string]bool{}
	for range 50 {
		out := startSession(t, h)
		if seen[out.UserCode] {
			t.Fatalf("duplicate user code %q", out.UserCode)
		}
		seen[out.UserCode] = true
	}
}

func TestPKCEVerifierIsPerSession(t *testing.T) {
	h := deviceHandler(t, "https://as/token")
	a := startSession(t, h)
	b := startSession(t, h)
	sa, _ := h.devices.byUserCode(a.UserCode)
	sb, _ := h.devices.byUserCode(b.UserCode)
	if sa.Verifier == sb.Verifier {
		t.Fatal("sessions share a PKCE verifier")
	}
	if oauth2.S256ChallengeFromVerifier(sa.Verifier) == "" {
		t.Fatal("verifier does not produce a challenge")
	}
}
