package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTOML(t *testing.T) {
	// Clear all LARD_ env vars to avoid interference
	clearLARDEnv()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `addr = ":8080"
db = "/tmp/lard.db"
memory_dir = "/tmp/memory"

[llm]
base_url = "https://api.openai.com/v1"
model = "gpt-4"
api_key = "sk-test123"

[auth]
mode = "oauth"
auth_server = "https://auth.example.com"
public_url = "https://lard.example.com"
allowed_client_ids = ["client1", "client2"]
allowed_users = ["user@example.com"]
required_scopes = ["profile", "email"]

[collector]
client_id = "collector-abc"
scopes = ["read", "write"]

[consolidate]
after = "10m"
max_wait = "1h"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Addr != ":8080" {
		t.Errorf("expected addr :8080, got %s", cfg.Addr)
	}
	if cfg.DB != "/tmp/lard.db" {
		t.Errorf("expected db /tmp/lard.db, got %s", cfg.DB)
	}
	if cfg.LLM.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", cfg.LLM.Model)
	}
	if cfg.Auth.Mode != "oauth" {
		t.Errorf("expected mode oauth, got %s", cfg.Auth.Mode)
	}
	if len(cfg.Auth.AllowedClientIDs) != 2 {
		t.Errorf("expected 2 allowed client IDs, got %d", len(cfg.Auth.AllowedClientIDs))
	}
	if cfg.Consolidate.After != "10m" {
		t.Errorf("expected consolidate after 10m, got %s", cfg.Consolidate.After)
	}
}

func TestEnvOverrides(t *testing.T) {
	clearLARDEnv()
	os.Setenv("LARD_ADDR", ":9090")
	defer os.Unsetenv("LARD_ADDR")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Addr != ":9090" {
		t.Errorf("expected addr :9090 from env, got %s", cfg.Addr)
	}
}

func TestDefaults(t *testing.T) {
	clearLARDEnv()
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Addr != ":7477" {
		t.Errorf("expected default addr :7477, got %s", cfg.Addr)
	}
	if cfg.Auth.Mode != "none" {
		t.Errorf("expected default auth mode none, got %s", cfg.Auth.Mode)
	}
	if cfg.Auth.AuthServerURL != "" {
		t.Errorf("expected empty auth server URL default, got %s", cfg.Auth.AuthServerURL)
	}
}

func TestConsolidateDurationParsing(t *testing.T) {
	c := &Consolidate{After: "5m", MaxWait: "30m"}
	if d := c.ConsolidateAfter(); d != 5*60*1e9 {
		t.Errorf("expected 5m duration, got %v", d)
	}
	if d := c.ConsolidateMaxWait(); d != 30*60*1e9 {
		t.Errorf("expected 30m duration, got %v", d)
	}

	// Test off/never/0
	c2 := &Consolidate{After: "off"}
	if d := c2.ConsolidateAfter(); d != 0 {
		t.Errorf("expected 0 for 'off', got %v", d)
	}
}

func TestSplitList(t *testing.T) {
	result := splitList("a, b, c")
	if len(result) != 3 {
		t.Errorf("expected 3 items, got %d", len(result))
	}
	if result[0] != "a" || result[1] != "b" || result[2] != "c" {
		t.Errorf("unexpected items: %v", result)
	}

	empty := splitList("")
	if empty != nil {
		t.Errorf("expected nil for empty string, got %v", empty)
	}
}

func clearLARDEnv() {
	envVars := []string{
		"LARD_ADDR", "LARD_DB", "LARD_MEMORY_DIR",
		"LARD_HYPER_BASE_URL", "LARD_MODEL", "LARD_HYPER_API_KEY", "OPENAI_API_VERSION",
		"LARD_AUTH", "LARD_TOKEN", "LARD_AUTH_SERVER", "LARD_PUBLIC_URL",
		"LARD_OAUTH_CLIENT_IDS", "LARD_OAUTH_USERS", "LARD_OAUTH_SCOPES",
		"LARD_COLLECTOR_CLIENT_ID", "LARD_COLLECTOR_SCOPES",
		"LARD_CONSOLIDATE_AFTER", "LARD_CONSOLIDATE_MAX_WAIT",
	}
	for _, v := range envVars {
		os.Unsetenv(v)
	}
}
