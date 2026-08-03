package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"
	"github.com/taciturnaxolotl/lard/internal/ui"
	"golang.org/x/oauth2"
)

// deviceGrantType is the RFC 8628 section 3.4 grant used when polling.
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Poll outcomes, matching RFC 8628 section 3.5's error codes.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errExpiredToken         = "expired_token"
	errAccessDenied         = "access_denied"
)

// LoginDevice authorizes this machine with the OAuth device authorization
// grant (RFC 8628), talking to the authorization server directly.
//
// No listener, no browser, and no client secret are needed on this machine:
// the device code itself is the proof of possession. The user approves the
// user code at the AS's verification URI from any browser anywhere, which is
// what makes login work unchanged over SSH, in a container, and on a headless
// box.
func LoginDevice(ctx context.Context, serverURL, clientID string, scopes []string, openBrowser bool) (*oauth2.Token, error) {
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	if eps.DeviceAuthorization == "" {
		return nil, errors.New("the authorization server does not offer the device grant")
	}
	if clientID == "" {
		if reg, err := FetchRegistration(ctx, serverURL); err == nil {
			clientID = reg.ClientID
		}
	}
	if clientID == "" {
		return nil, errors.New("no client id: the server publishes no collector registration")
	}

	auth, err := startDevice(ctx, eps.DeviceAuthorization, clientID, scopes)
	if err != nil {
		return nil, err
	}
	printDevicePrompt(auth, openBrowser)

	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second // RFC 8628 §3.5 default
	}
	deadline := time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second)
	if auth.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("this login expired; run 'lard-client login' again")
		}
		tok, code, err := pollDevice(ctx, eps.Token, clientID, auth.DeviceCode)
		if err != nil {
			return nil, err
		}
		switch code {
		case "":
			return tok, nil
		case errAuthorizationPending:
			// Still waiting on the human.
		case errSlowDown:
			// RFC 8628 §3.5: increase the interval by 5 seconds for this and
			// all subsequent requests.
			interval += 5 * time.Second
		case errExpiredToken:
			return nil, errors.New("this login expired; run 'lard-client login' again")
		case errAccessDenied:
			return nil, errors.New("authorization was declined")
		default:
			return nil, fmt.Errorf("authorization failed: %s", code)
		}
	}
}

// DeviceAuth mirrors the RFC 8628 device authorization response.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// startDevice asks the authorization server for a device/user code pair.
func startDevice(ctx context.Context, endpoint, clientID string, scopes []string) (*DeviceAuth, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{"client_id": {clientID}}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("starting authorization: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errors.New("authorization server is rate-limited; wait a moment and try again")
	}
	var auth DeviceAuth
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		return nil, fmt.Errorf("starting authorization: unreadable response (status %d)", resp.StatusCode)
	}
	if auth.DeviceCode == "" || auth.VerificationURI == "" {
		return nil, fmt.Errorf("starting authorization: server returned status %d", resp.StatusCode)
	}
	return &auth, nil
}

// pollDevice asks the token endpoint once whether authorization finished. A
// non-empty code string is an RFC 8628 status (pending, slow_down, ...)
// rather than a transport failure.
func pollDevice(ctx context.Context, tokenEndpoint, clientID, deviceCode string) (*oauth2.Token, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A transient network blip should not abandon a login the user is
		// halfway through, so treat it as "keep waiting".
		return nil, errAuthorizationPending, nil
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("polling: unreadable response (status %d)", resp.StatusCode)
	}
	if out.Error != "" {
		return nil, out.Error, nil
	}
	if out.AccessToken == "" {
		return nil, "", fmt.Errorf("polling: server returned status %d", resp.StatusCode)
	}
	tok := &oauth2.Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		TokenType:    "Bearer",
	}
	if out.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, "", nil
}

// isLocal guesses whether a browser on this machine is reachable by the user.
// SSH sets these, and their absence is the common case on a laptop.
func isLocal() bool {
	return os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_CLIENT") == "" && os.Getenv("SSH_TTY") == ""
}

// printDevicePrompt shows the URL and code.
//
// The URL is always printed, even when a browser opens, because opening one is
// a guess that is wrong over SSH and impossible on a headless machine. A
// printed URL costs one line and always works.
func printDevicePrompt(auth *DeviceAuth, openBrowser bool) {
	target := auth.VerificationURIComplete
	if target == "" {
		target = auth.VerificationURI
	}
	opened := false
	if openBrowser && isLocal() {
		opened = browser.OpenURL(target) == nil
	}
	if opened {
		fmt.Println("Opened your browser to authorize.")
		fmt.Println("If it opened on the wrong machine, use this link instead:")
	} else {
		fmt.Println("Open this link in any browser to authorize:")
	}
	fmt.Println()
	fmt.Println(ui.Link(target, "lard-device"))
	fmt.Println()
	if auth.UserCode != "" {
		fmt.Printf("Your code: %s\n", ui.Success(auth.UserCode))
	}
	fmt.Println("Waiting for you to authorize...")
}
