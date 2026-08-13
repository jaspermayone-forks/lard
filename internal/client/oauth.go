package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// DefaultScopes is what a collector asks for: enough to identify the user,
// plus offline_access so the authorization server issues a refresh token and
// the collector keeps syncing after the first access token expires instead of
// dying on the hour.
var DefaultScopes = []string{"profile", "offline_access"}

// Credentials is the OAuth client identity this machine holds. Every install
// registers its own (RFC 7591) rather than sharing one the server hands out,
// so revoking one machine leaves the others alone.
//
// ClientSecret is normally empty: lard-client registers as a public client.
// It is kept because RFC 7591 §3.2.1 lets an authorization server register
// something other than what was asked for, and one that issues a secret anyway
// expects to see it again.
type Credentials struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// form is how this client identifies itself in a request body: the id always,
// and a secret only if the server insisted on giving it one. Sending an empty
// client_secret would ask the authorization server to authenticate a value
// that isn't there, so an absent secret stays absent.
func (c Credentials) form() url.Values {
	form := url.Values{"client_id": {c.ClientID}}
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	return form
}

// Register claims a fresh OAuth client identity at the authorization server.
//
// It registers exactly the two grants it uses and no redirect_uris at all: a
// device client has nowhere to redirect, and RFC 7591 only wants that list for
// redirect-based grants.
//
// It registers public. A secret would be one more thing living in the same
// file as the refresh token, guarding against nothing that file's permissions
// don't already guard, and a login that has to be arranged rather than just
// run is the thing OAuth was supposed to remove. RFC 8628's device code is
// high-entropy proof of possession on its own, which is why the flow is
// specified for public clients in the first place.
func Register(ctx context.Context, endpoint, name string) (*Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"client_name":                name,
		"grant_types":                []string{deviceGrantType, "refresh_token"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registering this machine: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ClientID         string   `json:"client_id"`
		ClientSecret     string   `json:"client_secret"`
		GrantTypes       []string `json:"grant_types"`
		Error            string   `json:"error"`
		ErrorDescription string   `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("registering this machine: unreadable response (status %d)", resp.StatusCode)
	}
	if out.ClientID == "" {
		if out.ErrorDescription != "" {
			return nil, fmt.Errorf("registering this machine: %s", out.ErrorDescription)
		}
		return nil, fmt.Errorf("registering this machine: server returned status %d", resp.StatusCode)
	}
	// RFC 7591 §3.2.1 lets the server register something other than what was
	// asked for, so the response is the truth. Catching a missing device grant
	// here beats the same refusal arriving later as a confusing poll failure.
	if len(out.GrantTypes) > 0 && !slices.Contains(out.GrantTypes, deviceGrantType) {
		return nil, fmt.Errorf("the authorization server registered this machine for %v, which does not include the device grant",
			out.GrantTypes)
	}
	return &Credentials{ClientID: out.ClientID, ClientSecret: out.ClientSecret}, nil
}

// endpoints are the OAuth endpoints discovered from a lard server, plus the
// resource identifier that server publishes for itself.
type endpoints struct {
	Issuer              string
	Authorization       string
	Token               string
	DeviceAuthorization string
	Revocation          string
	Registration        string
	// Resource is lard's own identifier, sent as the RFC 8707 resource
	// indicator so the token comes back bound to this server and is useless
	// anywhere else. Taken from the server's own metadata rather than the URL
	// the user typed, since the two can differ by a trailing slash or a proxy.
	Resource string
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
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, base+"/.well-known/oauth-protected-resource", &prm); err != nil {
		return nil, fmt.Errorf("discover protected resource: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("server published no authorization servers")
	}
	as := strings.TrimRight(prm.AuthorizationServers[0], "/")
	resource := prm.Resource
	if resource == "" {
		resource = base
	}

	var meta struct {
		Issuer              string `json:"issuer"`
		Authorization       string `json:"authorization_endpoint"`
		Token               string `json:"token_endpoint"`
		DeviceAuthorization string `json:"device_authorization_endpoint"`
		Revocation          string `json:"revocation_endpoint"`
		Registration        string `json:"registration_endpoint"`
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
		Registration:        meta.Registration,
		Resource:            resource,
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
// a browser. creds are the client this machine registered as; RFC 6749 §6 has
// the authorization server re-authenticate it on every refresh. The audience
// rides along server-side, so the new token is bound to lard like the old one.
func RefreshToken(ctx context.Context, serverURL, refresh string, creds Credentials) (*oauth2.Token, error) {
	if refresh == "" {
		return nil, errors.New("no refresh token saved; run 'lard-client login'")
	}
	if creds.ClientID == "" {
		return nil, errors.New("no client id saved; run 'lard-client login' again")
	}
	eps, err := Discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	cfg := &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
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
