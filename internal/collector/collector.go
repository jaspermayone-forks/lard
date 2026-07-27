// Package collector serves the OAuth registration that edge collectors use to
// authenticate.
//
// The problem this solves: a collector cannot invent its own OAuth client id.
// The authorization server decides which clients exist, and lard decides which
// clients it trusts, so a client id guessed by the collector is one the server
// will reject. Instead the server publishes the identity to use, and the
// collector adopts it.
//
// When the registration is confidential (a client secret), the collector must
// not hold that secret: it is a public CLI on a laptop. So the server also
// performs the code exchange on the collector's behalf, keeping the secret
// server-side while the collector keeps its PKCE verifier. The collector proves
// possession with PKCE; the server proves the client's identity with the
// secret.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultPorts are the localhost callback ports a collector may bind, in
// preference order. They are published so the collector picks one the
// authorization server already knows, and validated so a stolen code cannot be
// redirected somewhere else.
var DefaultPorts = []int{40714, 40715, 40716, 40717, 40718}

// DefaultScopes is what a collector asks for: enough to identify the user.
var DefaultScopes = []string{"profile"}

// Config describes the collector OAuth registration this server hands out.
type Config struct {
	// ClientID is the OAuth client collectors authenticate as. Empty means no
	// registration is published, and collectors fall back to their own
	// localhost client id.
	ClientID string
	// ClientSecret, when set, marks the registration confidential. The server
	// then exchanges codes itself so the secret never leaves this process.
	ClientSecret string
	// Ports are the permitted localhost callback ports.
	Ports []int
	// Scopes the collector should request.
	Scopes []string
	// TokenEndpoint is the authorization server's token endpoint, used for the
	// server-side exchange.
	TokenEndpoint string
}

// Configured reports whether a registration is published.
func (c Config) Configured() bool { return c.ClientID != "" }

// Confidential reports whether the exchange must happen server-side.
func (c Config) Confidential() bool { return c.ClientSecret != "" }

func (c Config) ports() []int {
	if len(c.Ports) > 0 {
		return c.Ports
	}
	return DefaultPorts
}

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

// redirectURIs lists the callbacks the collector may use.
func (c Config) redirectURIs() []string {
	var out []string
	for _, p := range c.ports() {
		out = append(out, fmt.Sprintf("http://localhost:%d/callback", p))
	}
	return out
}

// Registration is the document a collector fetches before logging in.
type Registration struct {
	ClientID     string   `json:"clientId"`
	RedirectURIs []string `json:"redirectUris"`
	Scopes       []string `json:"scopes"`
	// ServerExchange tells the collector to post its code back here instead of
	// calling the token endpoint directly, because the secret lives on this
	// side.
	ServerExchange bool `json:"serverExchange"`
	// DeviceFlow means this server brokers the whole authorization: the
	// collector never runs a callback listener, so login works over SSH, in a
	// container, and on headless machines.
	DeviceFlow bool `json:"deviceFlow"`
}

// Handler serves the collector registration, the brokered device login, and
// (for confidential clients) the code exchange.
type Handler struct {
	cfg     Config
	dev     DeviceConfig
	prefix  string
	devices *deviceStore
}

// New builds the handler. prefix is the URL prefix the handler is mounted at,
// needed so the browser-facing URLs it builds are absolute.
func New(cfg Config, dev DeviceConfig, prefix string) *Handler {
	return &Handler{cfg: cfg, dev: dev, prefix: prefix, devices: newDeviceStore()}
}

// Register serves GET: the registration a collector should adopt.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Configured() {
		http.Error(w, `{"error":"no collector registration configured"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(Registration{
		ClientID:       h.cfg.ClientID,
		RedirectURIs:   h.cfg.redirectURIs(),
		Scopes:         h.cfg.scopes(),
		ServerExchange: h.cfg.Confidential(),
		DeviceFlow:     h.DeviceAvailable(),
	})
}

// exchangeRequest is what a collector posts after the user authorizes.
type exchangeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
}

// Exchange serves POST: trade an authorization code for a token using the
// server's confidential client credentials.
//
// The collector supplies the code and its PKCE verifier; this server supplies
// the client secret. Neither side alone can complete the exchange, which is the
// point.
func (h *Handler) Exchange(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Confidential() {
		http.Error(w, `{"error":"server-side exchange is not enabled"}`, http.StatusNotFound)
		return
	}
	var req exchangeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.Code == "" || req.CodeVerifier == "" {
		writeError(w, http.StatusBadRequest, "code and codeVerifier are required")
		return
	}
	// Only redirect back to a callback we published. Without this check the
	// endpoint would exchange a code for any redirect a caller names, which
	// turns the server's credentials into an oracle for stolen codes.
	if err := h.validateRedirect(req.RedirectURI); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tok, err := h.exchange(r.Context(), req)
	if err != nil {
		slog.Warn("collector: code exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	_ = json.NewEncoder(w).Encode(tok)
}

// validateRedirect confirms the redirect is one of the published localhost
// callbacks.
func (h *Handler) validateRedirect(raw string) error {
	if raw == "" {
		return errors.New("redirectUri is required")
	}
	if slices.Contains(h.cfg.redirectURIs(), raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("redirectUri is not a valid URL")
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil || (host != "localhost" && host != "127.0.0.1") {
		return errors.New("redirectUri must be a localhost callback")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || !slices.Contains(h.cfg.ports(), port) {
		return fmt.Errorf("redirectUri port must be one of %v", h.cfg.ports())
	}
	return nil
}

// TokenResponse is the subset of the token response a collector needs.
type TokenResponse struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	TokenType    string    `json:"tokenType,omitempty"`
}

func (h *Handler) exchange(ctx context.Context, req exchangeRequest) (*TokenResponse, error) {
	if h.cfg.TokenEndpoint == "" {
		return nil, errors.New("server has no token endpoint configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"client_id":     {h.cfg.ClientID},
		"client_secret": {h.cfg.ClientSecret},
		"redirect_uri":  {req.RedirectURI},
		"code_verifier": {req.CodeVerifier},
	}
	return postToken(ctx, h.cfg.TokenEndpoint, form)
}

// Refresh trades a refresh token for a new access token, again using the
// server's credentials so the collector never needs them.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Confidential() {
		http.Error(w, `{"error":"server-side refresh is not enabled"}`, http.StatusNotFound)
		return
	}
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refreshToken is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := postToken(ctx, h.cfg.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {req.RefreshToken},
		"client_id":     {h.cfg.ClientID},
		"client_secret": {h.cfg.ClientSecret},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	_ = json.NewEncoder(w).Encode(tok)
}

func postToken(ctx context.Context, endpoint string, form url.Values) (*TokenResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("reaching the token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("token endpoint returned unreadable JSON (status %d)", resp.StatusCode)
	}
	if body.Error != "" {
		if body.ErrorDescription != "" {
			return nil, fmt.Errorf("%s: %s", body.Error, body.ErrorDescription)
		}
		return nil, errors.New(body.Error)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	out := &TokenResponse{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
	}
	if body.ExpiresIn > 0 {
		out.Expiry = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return out, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
