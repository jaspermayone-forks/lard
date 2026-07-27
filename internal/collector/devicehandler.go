package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Device-flow paths, relative to the collector prefix.
const (
	PathDevice      = "/device"
	PathDeviceToken = "/device/token"
	PathVerify      = "/device/verify"
	PathCallback    = "/device/callback"
)

// DeviceConfig adds what the brokered flow needs beyond Config: where this
// server is reachable, and where to send users to authorize.
type DeviceConfig struct {
	// PublicURL is lard's externally reachable base URL. The OAuth redirect
	// comes back here, which is the whole point: a server URL is reachable
	// from wherever the user's browser is, and a collector's localhost is not.
	PublicURL string
	// AuthorizationEndpoint is the authorization server's authorize URL.
	AuthorizationEndpoint string
}

// DeviceAvailable reports whether the brokered flow can run. It needs a public
// URL to receive the redirect and a client id to authorize as.
func (h *Handler) DeviceAvailable() bool {
	return h.dev.PublicURL != "" && h.dev.AuthorizationEndpoint != "" && h.cfg.Configured()
}

// deviceStartResponse mirrors RFC 8628's device authorization response.
type deviceStartResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// StartDevice begins a brokered authorization.
//
// The client gets a device code to poll with and a URL to open. Because the URL
// lives on this server rather than on the collector's localhost, it works from
// any browser anywhere: over SSH, in a container, on a phone.
func (h *Handler) StartDevice(w http.ResponseWriter, r *http.Request) {
	if !h.DeviceAvailable() {
		writeError(w, http.StatusNotFound, "brokered device login is not configured on this server")
		return
	}
	verifier := oauth2.GenerateVerifier()
	sess, err := h.devices.create(verifier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start authorization")
		return
	}
	base := strings.TrimRight(h.dev.PublicURL, "/") + h.prefix
	verifyURI := base + PathVerify
	slog.Info("device login started", "user_code", sess.UserCode)
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	_ = json.NewEncoder(w).Encode(deviceStartResponse{
		DeviceCode:      sess.DeviceCode,
		UserCode:        sess.UserCode,
		VerificationURI: verifyURI,
		// The complete form carries the code, so the user confirms rather than
		// types. The plain form is the fallback for reading a code aloud.
		VerificationURIComplete: verifyURI + "?user_code=" + url.QueryEscape(sess.UserCode),
		ExpiresIn:               int(DeviceCodeTTL.Seconds()),
		Interval:                int(DevicePollInterval.Seconds()),
	})
}

// Verify is what the user's browser opens. It sends them on to the
// authorization server, binding this device session to the OAuth state.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	if !h.DeviceAvailable() {
		http.Error(w, "brokered device login is not configured", http.StatusNotFound)
		return
	}
	code := r.URL.Query().Get("user_code")
	if code == "" {
		h.devicePage(w, http.StatusOK, "Enter your code", userCodeForm(""))
		return
	}
	sess, ok := h.devices.byUserCode(code)
	if !ok {
		h.devicePage(w, http.StatusNotFound, "That code is not valid",
			"<p>It may have expired, or already been used. Start again with "+
				"<code>lard-client login</code>.</p>"+userCodeForm(""))
		return
	}
	http.Redirect(w, r, h.authorizeURL(sess), http.StatusFound)
}

// authorizeURL builds the authorization request, with the redirect pointing at
// this server.
func (h *Handler) authorizeURL(sess *deviceSession) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.cfg.ClientID},
		"redirect_uri":          {h.callbackURI()},
		"state":                 {sess.State},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(sess.Verifier)},
		"code_challenge_method": {"S256"},
		"scope":                 {strings.Join(h.cfg.scopes(), " ")},
		"access_type":           {"offline"},
	}
	sep := "?"
	if strings.Contains(h.dev.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return h.dev.AuthorizationEndpoint + sep + q.Encode()
}

// callbackURI is the redirect this server registers with the authorization
// server. Register exactly this with your provider.
func (h *Handler) callbackURI() string {
	return strings.TrimRight(h.dev.PublicURL, "/") + h.prefix + PathCallback
}

// CallbackURI exposes the redirect URI so it can be logged at boot: it is the
// one value an operator must register with the authorization server.
func (h *Handler) CallbackURI() string {
	if !h.DeviceAvailable() {
		return ""
	}
	return h.callbackURI()
}

// Callback receives the authorization server's redirect, exchanges the code
// using this server's credentials, and parks the token for the polling client.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.DeviceAvailable() {
		http.Error(w, "brokered device login is not configured", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	sess, ok := h.devices.byStateCode(q.Get("state"))
	if !ok {
		// No session for this state: either it expired or the state was forged.
		// Either way there is nothing to complete.
		h.devicePage(w, http.StatusBadRequest, "This login has expired",
			"<p>Start again with <code>lard-client login</code>.</p>")
		return
	}
	if e := q.Get("error"); e != "" {
		h.devices.complete(sess, nil, ErrAccessDenied)
		h.devicePage(w, http.StatusOK, "Authorization declined",
			"<p>Nothing was connected. You can close this tab.</p>")
		return
	}
	code := q.Get("code")
	if code == "" {
		h.devices.complete(sess, nil, ErrInvalidGrant)
		h.devicePage(w, http.StatusBadRequest, "No authorization code",
			"<p>The provider did not return a code. Try again.</p>")
		return
	}
	tok, err := h.exchangeDevice(r.Context(), code, sess.Verifier)
	if err != nil {
		slog.Warn("device login: exchange failed", "error", err)
		h.devices.complete(sess, nil, err.Error())
		h.devicePage(w, http.StatusBadGateway, "Could not finish authorizing",
			"<p>"+html.EscapeString(err.Error())+"</p>")
		return
	}
	h.devices.complete(sess, tok, "")
	slog.Info("device login completed", "user_code", sess.UserCode)
	h.devicePage(w, http.StatusOK, "Connected",
		"<p>This machine is now connected to lard. You can close this tab and return to your terminal.</p>")
}

// exchangeDevice trades the code for a token. The client secret is used when
// there is one, which is why this happens here rather than on the collector.
func (h *Handler) exchangeDevice(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.cfg.ClientID},
		"redirect_uri":  {h.callbackURI()},
		"code_verifier": {verifier},
	}
	if h.cfg.ClientSecret != "" {
		form.Set("client_secret", h.cfg.ClientSecret)
	}
	res, err := postToken(ctx, h.cfg.TokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		Expiry:       res.Expiry,
		TokenType:    "Bearer",
	}, nil
}

// PollDevice is the client's polling endpoint. It answers with the token once
// the user finishes, and with RFC 8628 error codes until then.
func (h *Handler) PollDevice(w http.ResponseWriter, r *http.Request) {
	if !h.DeviceAvailable() {
		writeError(w, http.StatusNotFound, "brokered device login is not configured")
		return
	}
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.DeviceCode == "" {
		writeError(w, http.StatusBadRequest, "deviceCode is required")
		return
	}
	tok, errCode := h.devices.poll(req.DeviceCode)
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	if errCode != "" {
		// Pending and slow_down are normal progress, not failures, so they get
		// 200 with an error code the client understands. Anything else is a
		// real 400.
		status := http.StatusBadRequest
		if errCode == ErrAuthorizationPending || errCode == ErrSlowDown {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode})
		return
	}
	_ = json.NewEncoder(w).Encode(TokenResponse{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		TokenType:    "Bearer",
	})
}

// devicePage renders a minimal styled page for the browser side of the flow.
func (h *Handler) devicePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><title>%s · lard</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;line-height:1.5;
      max-width:34rem;margin:0 auto;padding:3rem 1.5rem;color:#1c1917}
 h1{font-size:1.25rem;margin:0 0 1rem}
 code{background:#f5f5f4;padding:.15em .4em;border-radius:3px}
 input{font:inherit;padding:.6rem;width:11rem;letter-spacing:.15em;
       text-transform:uppercase;border:2px solid #1c1917;border-radius:4px}
 button{font:inherit;padding:.6rem 1.2rem;margin-left:.5rem;cursor:pointer;
        border:2px solid #1c1917;border-radius:4px;background:#1c1917;color:#fafaf9}
 @media(prefers-color-scheme:dark){
   body{background:#1c1917;color:#fafaf9}code{background:#292524}
   input{border-color:#fafaf9;background:#292524;color:#fafaf9}
   button{background:#fafaf9;color:#1c1917;border-color:#fafaf9}}
</style></head><body><h1>%s</h1>%s</body></html>`,
		html.EscapeString(title), html.EscapeString(title), body)
}

// userCodeForm is the fallback for a user who read their code off another
// screen rather than following a link.
func userCodeForm(prefill string) string {
	return `<form method="get"><label>Code<br><input name="user_code" value="` +
		html.EscapeString(prefill) + `" placeholder="XXXX-XXXX" autofocus autocomplete="off" spellcheck="false">` +
		`</label><button type="submit">Continue</button></form>`
}
