// Package config holds the server's configuration, loaded from TOML and
// overridden by environment variables.
package config

import (
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Server holds all runtime configuration for the lard server.
type Server struct {
	Addr        string      `toml:"addr" env:"LARD_ADDR" default:":7477"`
	DB          string      `toml:"db" env:"LARD_DB"`
	MemoryDir   string      `toml:"memory_dir" env:"LARD_MEMORY_DIR"`
	LLM         LLM         `toml:"llm"`
	Auth        Auth        `toml:"auth"`
	Collector   Collector   `toml:"collector"`
	Consolidate Consolidate `toml:"consolidate"`
}

// LLM holds OpenAI-compatible client settings.
type LLM struct {
	BaseURL    string `toml:"base_url" env:"OPENAI_BASE_URL"`
	Model      string `toml:"model" env:"OPENAI_MODEL"`
	APIKey     string `toml:"api_key" env:"OPENAI_API_KEY"`
	APIVersion string `toml:"api_version" env:"OPENAI_API_VERSION"`
}

// Auth holds authentication configuration.
type Auth struct {
	Mode              string   `toml:"mode" env:"LARD_AUTH" default:"none"`
	Token             string   `toml:"token" env:"LARD_TOKEN"`
	IndikoURL         string   `toml:"indiko_url" env:"LARD_INDIKO_URL" default:"https://indiko.dunkirk.sh"`
	PublicURL         string   `toml:"public_url" env:"LARD_PUBLIC_URL"`
	AllowedClientIDs  []string `toml:"allowed_client_ids" env:"LARD_OAUTH_CLIENT_IDS"`
	AllowedUsers      []string `toml:"allowed_users" env:"LARD_OAUTH_USERS"`
	RequiredScopes    []string `toml:"required_scopes" env:"LARD_OAUTH_SCOPES"`
	CollectorClientID string   `toml:"collector_client_id" env:"LARD_COLLECTOR_CLIENT_ID"`
}

// Collector holds the collector OAuth registration.
type Collector struct {
	ClientID string   `toml:"client_id" env:"LARD_COLLECTOR_CLIENT_ID"`
	Scopes   []string `toml:"scopes" env:"LARD_COLLECTOR_SCOPES"`
}

// Consolidate holds auto-consolidation settings.
type Consolidate struct {
	After   string `toml:"after" env:"LARD_CONSOLIDATE_AFTER" default:"5m"`
	MaxWait string `toml:"max_wait" env:"LARD_CONSOLIDATE_MAX_WAIT" default:"30m"`
}

// Load reads configuration from a TOML file, then applies environment variable
// overrides. Env vars always win.
func Load(path string) (*Server, error) {
	cfg := defaults()

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := toml.Unmarshal(b, cfg); err != nil {
				return nil, err
			}
		}
	}

	applyEnv(cfg)
	return cfg, nil
}

func defaults() *Server {
	cfg := &Server{}
	applyDefaults(reflect.ValueOf(cfg).Elem())
	return cfg
}

// applyDefaults recursively walks a struct and applies default values
// based on `default` tags.
func applyDefaults(v reflect.Value) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fieldVal := v.Field(i)

		// Handle nested structs
		if fieldVal.Kind() == reflect.Struct {
			applyDefaults(fieldVal)
			continue
		}

		defaultTag := field.Tag.Get("default")
		if defaultTag == "" {
			continue
		}

		// Only set if the field is empty (zero value)
		isZero := false
		switch fieldVal.Kind() {
		case reflect.String:
			isZero = fieldVal.String() == ""
		case reflect.Slice:
			isZero = fieldVal.Len() == 0
		}

		if !isZero {
			continue
		}

		// Set the default value based on type
		switch fieldVal.Kind() {
		case reflect.String:
			fieldVal.SetString(defaultTag)
		case reflect.Slice:
			if fieldVal.Type().Elem().Kind() == reflect.String {
				fieldVal.Set(reflect.ValueOf(splitList(defaultTag)))
			}
		}
	}
}

func applyEnv(cfg *Server) {
	applyEnvToStruct(reflect.ValueOf(cfg).Elem())
}

// applyEnvToStruct recursively walks a struct and applies env var overrides
// based on `env` tags.
func applyEnvToStruct(v reflect.Value) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fieldVal := v.Field(i)

		// Handle nested structs
		if fieldVal.Kind() == reflect.Struct {
			applyEnvToStruct(fieldVal)
			continue
		}

		envTag := field.Tag.Get("env")
		if envTag == "" {
			continue
		}

		val := os.Getenv(envTag)
		if val == "" {
			continue
		}

		// Set the field value based on its type
		switch fieldVal.Kind() {
		case reflect.String:
			fieldVal.SetString(val)
		case reflect.Slice:
			if fieldVal.Type().Elem().Kind() == reflect.String {
				fieldVal.Set(reflect.ValueOf(splitList(val)))
			}
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ConsolidateAfter parses the after duration.
func (c *Consolidate) ConsolidateAfter() time.Duration {
	if c.After == "" || c.After == "off" || c.After == "never" || c.After == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.After)
	if err != nil {
		return 0
	}
	return d
}

// ConsolidateMaxWait parses the max wait duration.
func (c *Consolidate) ConsolidateMaxWait() time.Duration {
	if c.MaxWait == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(c.MaxWait)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}
