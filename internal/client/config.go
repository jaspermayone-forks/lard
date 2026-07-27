// Package client also owns the collector's own configuration: where the
// central lard lives and how to authenticate to it.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/taciturnaxolotl/lard/internal/xdg"
)

// Config is the collector's persisted setup. It exists so a background daemon
// needs no environment at all: launchd and systemd both make per-service env
// vars awkward, and a file the user can inspect beats an invisible plist.
type Config struct {
	URL   string   `json:"url"`
	Roots []string `json:"roots,omitempty"`
	// Token is a static shared secret, for a server running LARD_AUTH=token
	// or a headless box where no browser is available.
	Token string `json:"token,omitempty"`
	// OAuth holds the browser-login credentials, used when the server runs
	// LARD_AUTH=bearer. Preferred over Token: nothing to copy by hand, and it
	// carries the same identity as the rest of the user's tooling.
	OAuth *OAuthToken `json:"oauth,omitempty"`
}

// OAuthToken is the persisted result of `lard-client login`.
type OAuthToken struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	// ClientID is the public OAuth client this token was minted for. A refresh
	// must present the same id, so it is pinned here rather than re-derived.
	ClientID string `json:"clientId,omitempty"`
}

// expired reports whether the access token is gone or about to lapse. The
// minute of slack keeps a long upload from dying mid-flight.
func (t *OAuthToken) expired() bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return time.Now().After(t.Expiry.Add(-time.Minute))
}

// DefaultConfigPath is ~/.config/lard/client.toml.
func DefaultConfigPath() string {
	return xdg.ConfigPath("client.toml")
}

// LoadConfig reads the config file, tolerating absence, then lets the
// environment override it. Env wins so a one-off run can point somewhere else
// without editing the file.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := toml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if v := os.Getenv("LARD_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("LARD_TOKEN"); v != "" {
		cfg.Token = v
	}
	if cfg.URL == "" {
		cfg.URL = "http://localhost:7477"
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return cfg, nil
}

// Save writes the config atomically with owner-only permissions, since it
// holds a bearer token.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Verify checks that the configured URL and token actually work, so setup
// fails loudly at install time rather than silently in a background daemon.
// It returns the identity the server reports, when it reports one.
func (c *Config) Verify(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/whoami", nil)
	if err != nil {
		return "", err
	}
	if tok, err := c.Bearer(ctx, DefaultConfigPath()); err == nil && tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", errors.New("server rejected the token (401); check LARD_TOKEN matches the server's")
	case resp.StatusCode == http.StatusForbidden:
		return "", errors.New("token is valid but not allowed for this server (403); check the server's allowlist")
	case resp.StatusCode == http.StatusNotFound:
		// An older server without /whoami. Reaching it at all is enough.
		return "", nil
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("unexpected status %d from %s/whoami", resp.StatusCode, c.URL)
	}
	var body struct {
		Subject  string `json:"subject"`
		ClientID string `json:"clientId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Subject != "" {
		return body.Subject, nil
	}
	return body.ClientID, nil
}

// Bearer returns the token to send, refreshing an expired OAuth access token
// and persisting the new one. It is the single place that decides between
// OAuth and a static secret, so callers never branch on auth mode.
//
// A refresh failure is fatal by design: silently falling back to no
// credentials would turn an expired login into a stream of 401s in a log file
// nobody reads.
func (c *Config) Bearer(ctx context.Context, path string) (string, error) {
	if c.OAuth != nil && c.OAuth.AccessToken != "" {
		if !c.OAuth.expired() {
			return c.OAuth.AccessToken, nil
		}
		tok, err := RefreshToken(ctx, c.URL, c.OAuth.RefreshToken, c.OAuth.ClientID)
		if err != nil {
			return "", err
		}
		c.OAuth.AccessToken = tok.AccessToken
		c.OAuth.Expiry = tok.Expiry
		// Indiko does not rotate refresh tokens, but honor one if it appears.
		if tok.RefreshToken != "" {
			c.OAuth.RefreshToken = tok.RefreshToken
		}
		if err := c.Save(path); err != nil {
			return "", fmt.Errorf("save refreshed token: %w", err)
		}
		return c.OAuth.AccessToken, nil
	}
	return c.Token, nil
}

// AuthMode names how this config authenticates, for status output.
func (c *Config) AuthMode() string {
	switch {
	case c.OAuth != nil && c.OAuth.AccessToken != "":
		return "oauth"
	case c.Token != "":
		return "token"
	default:
		return "none"
	}
}
