package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

// CallbackPort is the localhost port the login flow listens on by default.
// Crush uses 40704-40713 for its own MCP flow, so this sits just past that
// range. The server may publish a different set, which wins.
const CallbackPort = 40714

// ClientID is the fallback OAuth client id, used only when the server publishes
// no collector registration. Deriving it from the callback port satisfies
// authorization servers that require the client id host to match the redirect
// host, but a server-published id is always preferred: the server is the one
// that decides which clients it trusts.
func ClientID(port int) string {
	return fmt.Sprintf("http://localhost:%d/", port)
}

// Registration is the OAuth client identity a lard server tells collectors to
// use, fetched from its collector endpoint.
type Registration struct {
	ClientID     string   `json:"clientId"`
	RedirectURIs []string `json:"redirectUris"`
	Scopes       []string `json:"scopes"`
	// ServerExchange means the server holds a client secret and will trade the
	// authorization code for a token on our behalf, so the secret stays off
	// this machine.
	ServerExchange bool `json:"serverExchange"`
	// DeviceFlow means the server brokers the whole authorization, so this
	// machine needs no callback listener and no browser of its own.
	DeviceFlow bool `json:"deviceFlow"`
}

// Ports extracts the callback ports from the published redirect URIs.
func (r *Registration) Ports() []int {
	var out []int
	for _, raw := range r.RedirectURIs {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if _, portStr, err := net.SplitHostPort(u.Host); err == nil {
			if n, err := strconv.Atoi(portStr); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// FetchRegistration asks the server which OAuth client to be. A 404 means the
// server publishes none, and the caller falls back to a localhost client id.
func FetchRegistration(ctx context.Context, serverURL string) (*Registration, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var reg Registration
	if err := getJSON(ctx, strings.TrimRight(serverURL, "/")+"/auth/collector", &reg); err != nil {
		return nil, err
	}
	if reg.ClientID == "" {
		return nil, errors.New("server published an empty client id")
	}
	return &reg, nil
}

// endpoints are the OAuth endpoints discovered from a lard server.
type endpoints struct {
	Issuer        string
	Authorization string
	Token         string
}

// Discover walks the RFC 9728 -> RFC 8414 chain: ask the lard server which
// authorization server protects it, then ask that server where its endpoints
// are. Nothing about indiko is hardcoded, so pointing lard at a different
// provider needs no client change.
func Discover(ctx context.Context, serverURL string) (*endpoints, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	base := strings.TrimRight(serverURL, "/")

	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, base+"/.well-known/oauth-protected-resource", &prm); err != nil {
		return nil, fmt.Errorf("discover protected resource: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("server published no authorization servers")
	}
	as := strings.TrimRight(prm.AuthorizationServers[0], "/")

	var meta struct {
		Issuer        string `json:"issuer"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	if err := getJSON(ctx, as+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return nil, fmt.Errorf("discover authorization server: %w", err)
	}
	if meta.Authorization == "" || meta.Token == "" {
		return nil, errors.New("authorization server metadata is missing endpoints")
	}
	return &endpoints{Issuer: meta.Issuer, Authorization: meta.Authorization, Token: meta.Token}, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Login runs the browser authorization-code flow with PKCE and returns the
// token. The caller persists it.
// Login runs the browser authorization-code flow with PKCE and returns the
// token.
//
// The OAuth client identity comes from the server when it publishes one, since
// the server is what decides which clients it accepts. A client id guessed here
// would be rejected by the very server we are trying to reach.
func Login(ctx context.Context, serverURL string, port int, openBrowser bool) (*oauth2.Token, error) {
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}

	clientID := ""
	scopes := []string{"profile"}
	serverExchange := false
	ports := []int{}
	if reg, err := FetchRegistration(ctx, serverURL); err == nil {
		clientID = reg.ClientID
		serverExchange = reg.ServerExchange
		if len(reg.Scopes) > 0 {
			scopes = reg.Scopes
		}
		ports = reg.Ports()
	} else {
		slog.Debug("no collector registration published; using a localhost client id", "error", err)
	}
	// An explicitly requested port wins, then the server's list, then the
	// default. The chosen port must appear in the server's list or it will
	// refuse the exchange.
	if port > 0 {
		ports = append([]int{port}, ports...)
	}
	if len(ports) == 0 {
		ports = []int{CallbackPort}
	}

	// Bind before sending the user to the browser, so a busy port fails now
	// rather than after they have authenticated.
	ln, boundPort, err := listenAny(ports)
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	if clientID == "" {
		clientID = ClientID(boundPort)
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", boundPort)
	cfg := &oauth2.Config{
		ClientID:    clientID,
		RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{
			AuthURL:   eps.Authorization,
			TokenURL:  eps.Token,
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: scopes,
	}

	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOffline, // ask for a refresh token
	)

	// Over SSH the local browser is the wrong browser, so don't try to launch
	// one: the printed URL is what the user actually needs.
	code, err := awaitCode(ctx, ln, state, authURL, openBrowser && isLocal())
	if err != nil {
		return nil, err
	}

	// A confidential client's secret lives on the server, so the server
	// finishes the exchange. We still hold the PKCE verifier, so neither side
	// can complete it alone.
	if serverExchange {
		return exchangeViaServer(ctx, serverURL, code, verifier, redirectURI)
	}
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange code for token: %w", err)
	}
	return tok, nil
}

// listenAny binds the first available port from the list.
func listenAny(ports []int) (net.Listener, int, error) {
	var lastErr error
	for _, p := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no callback port available (tried %v): %w", ports, lastErr)
}

// awaitCode serves the redirect callback and returns the authorization code.
func awaitCode(ctx context.Context, ln net.Listener, state, authURL string, openBrowser bool) (string, error) {
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case q.Get("error") != "":
				finish(w, "Authorization failed: "+q.Get("error"))
				results <- result{err: fmt.Errorf("authorization denied: %s", q.Get("error"))}
			case q.Get("state") != state:
				finish(w, "Authorization failed: state mismatch.")
				results <- result{err: errors.New("state mismatch; possible CSRF, aborting")}
			case q.Get("code") == "":
				finish(w, "Authorization failed: no code returned.")
				results <- result{err: errors.New("no authorization code in callback")}
			default:
				finish(w, "Connected. You can close this tab and return to the terminal.")
				results <- result{code: q.Get("code")}
			}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	printAuthURL(authURL, listenPort(ln), openBrowser)

	// Give the human a few minutes to find their passkey.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	select {
	case <-waitCtx.Done():
		return "", errors.New("timed out waiting for authorization")
	case res := <-results:
		return res.code, res.err
	}
}

// exchangeViaServer asks lard to trade the code for a token using its own
// confidential client credentials.
func exchangeViaServer(ctx context.Context, serverURL, code, verifier, redirectURI string) (*oauth2.Token, error) {
	body, err := json.Marshal(map[string]string{
		"code":         code,
		"codeVerifier": verifier,
		"redirectUri":  redirectURI,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/auth/collector/exchange", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server-side exchange: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string    `json:"accessToken"`
		RefreshToken string    `json:"refreshToken"`
		Expiry       time.Time `json:"expiry"`
		Error        string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("server-side exchange returned unreadable JSON (status %d)", resp.StatusCode)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("server-side exchange: %s", out.Error)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("server-side exchange returned status %d", resp.StatusCode)
	}
	return &oauth2.Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		Expiry:       out.Expiry,
		TokenType:    "Bearer",
	}, nil
}

func finish(w http.ResponseWriter, msg string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><title>lard</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:ui-monospace,SFMono-Regular,monospace;padding:3rem;max-width:34rem;line-height:1.5">
<p>%s</p></body></html>`, html.EscapeString(msg))
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RefreshToken exchanges a refresh token for a fresh access token. The daemon
// uses this, since it must never try to open a browser.
//
// A confidential registration is refreshed through the server for the same
// reason its code was exchanged there: the client secret is required and lives
// only on the server.
func RefreshToken(ctx context.Context, serverURL, refresh string, port int) (*oauth2.Token, error) {
	if refresh == "" {
		return nil, errors.New("no refresh token saved; run 'lard-client login'")
	}
	if reg, err := FetchRegistration(ctx, serverURL); err == nil && reg.ServerExchange {
		return refreshViaServer(ctx, serverURL, refresh)
	}
	if port <= 0 {
		port = CallbackPort
	}
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	clientID := ClientID(port)
	if reg, err := FetchRegistration(ctx, serverURL); err == nil {
		clientID = reg.ClientID
	}
	cfg := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:   eps.Authorization,
			TokenURL:  eps.Token,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	tok, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token rejected (re-run 'lard-client login'): %w", err)
	}
	return tok, nil
}

// refreshViaServer asks lard to refresh using its confidential credentials.
func refreshViaServer(ctx context.Context, serverURL, refresh string) (*oauth2.Token, error) {
	body, err := json.Marshal(map[string]string{"refreshToken": refresh})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/auth/collector/refresh", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server-side refresh: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string    `json:"accessToken"`
		RefreshToken string    `json:"refreshToken"`
		Expiry       time.Time `json:"expiry"`
		Error        string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("server-side refresh returned unreadable JSON (status %d)", resp.StatusCode)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("server-side refresh: %s (re-run 'lard-client login')", out.Error)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("server-side refresh returned status %d", resp.StatusCode)
	}
	tok := &oauth2.Token{AccessToken: out.AccessToken, Expiry: out.Expiry, TokenType: "Bearer"}
	// Keep the existing refresh token when the server does not rotate it.
	tok.RefreshToken = out.RefreshToken
	if tok.RefreshToken == "" {
		tok.RefreshToken = refresh
	}
	return tok, nil
}

// printAuthURL shows the authorization URL and, always, the raw URL itself.
//
// The URL is printed even when a browser opens successfully. Opening a browser
// is a guess: over SSH it launches on the wrong machine, in a container it
// launches nowhere, and a headless box has none. Printing the URL costs one
// screen of text and is the difference between a working login and a dead end.
func printAuthURL(authURL string, port int, openBrowser bool) {
	opened := false
	if openBrowser {
		opened = browser.OpenURL(authURL) == nil
	}
	if opened {
		fmt.Println("Opened your browser to authorize.")
		fmt.Println("If it opened on the wrong machine, use this URL instead:")
	} else {
		fmt.Println("Open this URL to authorize:")
	}
	// Bare, on its own line, unwrapped and unstyled: terminals turn a lone URL
	// into a click target, and anything decorative breaks copy and paste.
	fmt.Printf("\n%s\n\n", authURL)
	fmt.Printf("Waiting for the callback on localhost:%d...\n", port)
	if !isLocal() {
		fmt.Printf("\nThis looks like a remote session. The callback goes to localhost:%d\n", port)
		fmt.Printf("on *this* machine, so forward it from your laptop first:\n")
		fmt.Printf("  ssh -L %d:localhost:%d %s\n", port, port, remoteHostHint())
	}
}

// listenPort reports the port a listener is bound to.
func listenPort(ln net.Listener) int {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// isLocal guesses whether a browser on this machine is reachable by the user.
// SSH sets these, and their absence is the common case on a laptop.
func isLocal() bool {
	return os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_CLIENT") == "" && os.Getenv("SSH_TTY") == ""
}

// remoteHostHint names this host for the suggested ssh command.
func remoteHostHint() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this-host"
}
