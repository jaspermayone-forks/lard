package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.toml")
	content := `url = "https://lard.example.com"
roots = ["/home/user/code", "/home/user/projects"]
token = "secret-token"

[oauth]
accessToken = "test-access-token"
refreshToken = "test-refresh-token"
clientId = "test-client-id"
expiry = 2024-01-15T10:30:00Z
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.URL != "https://lard.example.com" {
		t.Errorf("expected URL https://lard.example.com, got %s", cfg.URL)
	}
	if len(cfg.Roots) != 2 {
		t.Errorf("expected 2 roots, got %d", len(cfg.Roots))
	}
	if cfg.Token != "secret-token" {
		t.Errorf("expected token secret-token, got %s", cfg.Token)
	}
	if cfg.OAuth == nil {
		t.Fatal("expected OAuth config")
	}
	if cfg.OAuth.AccessToken != "test-access-token" {
		t.Errorf("expected access token test-access-token, got %s", cfg.OAuth.AccessToken)
	}
}

func TestSaveConfigTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.toml")

	cfg := &Config{
		URL:   "https://lard.example.com",
		Roots: []string{"/home/user/code"},
		Token: "test-token",
		OAuth: &OAuthToken{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			Credentials:  Credentials{ClientID: "client-id", ClientSecret: "client-secret"},
		},
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loadedCfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if loadedCfg.URL != cfg.URL {
		t.Errorf("expected URL %s, got %s", cfg.URL, loadedCfg.URL)
	}
	if len(loadedCfg.Roots) != len(cfg.Roots) {
		t.Errorf("expected %d roots, got %d", len(cfg.Roots), len(loadedCfg.Roots))
	}
	if loadedCfg.Token != cfg.Token {
		t.Errorf("expected token %s, got %s", cfg.Token, loadedCfg.Token)
	}
	// The client identity has to survive the round trip, or a refresh after a
	// restart has nothing to authenticate with.
	if loadedCfg.OAuth == nil || loadedCfg.OAuth.Credentials != cfg.OAuth.Credentials {
		t.Errorf("expected credentials %+v, got %+v", cfg.OAuth.Credentials, loadedCfg.OAuth)
	}
}
