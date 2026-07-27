package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

// Poll outcomes, matching RFC 8628's error codes.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errExpiredToken         = "expired_token"
	errAccessDenied         = "access_denied"
)

// DeviceAuth is what the server hands back when a brokered login starts.
type DeviceAuth struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// LoginDevice authorizes this machine through the server rather than through a
// local callback listener.
//
// The collector opens no ports and needs no browser of its own: the user visits
// a URL on the lard server from any browser anywhere, and this function polls
// until the server reports a token. That is what makes login work unchanged over
// SSH, inside a container, and on a headless box.
func LoginDevice(ctx context.Context, serverURL string, openBrowser bool) (*oauth2.Token, error) {
	auth, err := startDevice(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	printDevicePrompt(auth, openBrowser)

	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
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
		tok, code, err := pollDevice(ctx, serverURL, auth.DeviceCode)
		if err != nil {
			return nil, err
		}
		switch code {
		case "":
			fmt.Println("Connected.")
			return tok, nil
		case errAuthorizationPending:
			// Still waiting on the human.
		case errSlowDown:
			// The server says we are polling too fast; back off permanently
			// rather than just for this round.
			interval += time.Second
		case errExpiredToken:
			return nil, errors.New("this login expired; run 'lard-client login' again")
		case errAccessDenied:
			return nil, errors.New("authorization was declined")
		default:
			return nil, fmt.Errorf("authorization failed: %s", code)
		}
	}
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
		fmt.Println("Opened your browser to authorize. If it opened on the wrong")
		fmt.Println("machine, use this URL from any browser instead:")
	} else {
		fmt.Println("Open this URL in any browser to authorize:")
	}
	// Bare and unstyled on its own line: terminals linkify a lone URL, and
	// decoration breaks copy and paste.
	fmt.Printf("\n%s\n\n", target)
	if auth.UserCode != "" {
		fmt.Printf("Your code: %s\n", auth.UserCode)
	}
	fmt.Println("Waiting for you to authorize...")
}

func startDevice(ctx context.Context, serverURL string) (*DeviceAuth, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/auth/collector/device", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("starting authorization: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("this server does not offer brokered login")
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

// pollDevice asks once whether authorization finished. A non-empty code string
// is a status (pending, slow_down, ...) rather than a transport failure.
func pollDevice(ctx context.Context, serverURL, deviceCode string) (*oauth2.Token, string, error) {
	body, err := json.Marshal(map[string]string{"deviceCode": deviceCode})
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/auth/collector/device/token", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A transient network blip should not abandon a login the user is
		// halfway through, so treat it as "keep waiting".
		return nil, errAuthorizationPending, nil
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string    `json:"accessToken"`
		RefreshToken string    `json:"refreshToken"`
		Expiry       time.Time `json:"expiry"`
		Error        string    `json:"error"`
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
	return &oauth2.Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		Expiry:       out.Expiry,
		TokenType:    "Bearer",
	}, "", nil
}
