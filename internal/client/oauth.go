package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Registration is the OAuth client identity a lard server tells collectors to
// use, fetched from its collector endpoint.
type Registration struct {
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes"`
}

// FetchRegistration asks the server which OAuth client to be. A 404 means the
// server publishes none.
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
	Issuer              string
	Authorization       string
	Token               string
	DeviceAuthorization string
	Revocation          string
}

// Discover walks the RFC 9728 -> RFC 8414 chain: ask the lard server which
// authorization server protects it, then ask that server where its endpoints
// are. Nothing about the provider is hardcoded, so pointing lard at a
// different authorization server needs no client change.
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
		Issuer              string `json:"issuer"`
		Authorization       string `json:"authorization_endpoint"`
		Token               string `json:"token_endpoint"`
		DeviceAuthorization string `json:"device_authorization_endpoint"`
		Revocation          string `json:"revocation_endpoint"`
	}
	if err := getJSON(ctx, as+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return nil, fmt.Errorf("discover authorization server: %w", err)
	}
	if meta.Authorization == "" || meta.Token == "" {
		return nil, errors.New("authorization server metadata is missing endpoints")
	}
	return &endpoints{
		Issuer:              meta.Issuer,
		Authorization:       meta.Authorization,
		Token:               meta.Token,
		DeviceAuthorization: meta.DeviceAuthorization,
		Revocation:          meta.Revocation,
	}, nil
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

// RefreshToken exchanges a refresh token for a fresh access token at the
// authorization server. The daemon uses this, since it must never try to open
// a browser. clientID is the public client the refresh token was minted for.
func RefreshToken(ctx context.Context, serverURL, refresh, clientID string) (*oauth2.Token, error) {
	if refresh == "" {
		return nil, errors.New("no refresh token saved; run 'lard-client login'")
	}
	if clientID == "" {
		return nil, errors.New("no client id saved; run 'lard-client login' again")
	}
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return nil, err
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

// RevokeToken tells the authorization server to kill a token (RFC 7009), so a
// logged-out machine's refresh token stops working anywhere, not just locally.
// Best-effort: the server answers 200 even for a token it doesn't know.
func RevokeToken(ctx context.Context, serverURL, token string) error {
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return err
	}
	if eps.Revocation == "" {
		return errors.New("the authorization server does not offer token revocation")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.Revocation,
		strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoking token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking token: status %d", resp.StatusCode)
	}
	return nil
}
