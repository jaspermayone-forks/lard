// lard is the central memory service: HTTP API + MCP over one SQLite store.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/taciturnaxolotl/lard/internal/auth"
	"github.com/taciturnaxolotl/lard/internal/collector"
	"github.com/taciturnaxolotl/lard/internal/dotenv"
	"github.com/taciturnaxolotl/lard/internal/httpapi"
	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/mcpserver"
	"github.com/taciturnaxolotl/lard/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lard:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load .env before reading any config: auth mode lives there too, and a
	// missed load would silently leave the service open.
	dotenv.LoadDefault()

	dbPath := envOr("LARD_DB", defaultDBPath())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return err
	}
	memDir := envOr("LARD_MEMORY_DIR", defaultMemDir())
	st, err := store.Open(dbPath, memDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// The LLM client is optional at boot: the API works without it, only
	// /consolidate refuses.
	var llmClient *llm.Client
	if c, err := llm.NewFromEnv(ctx); err != nil {
		slog.Warn("no LLM client; /consolidate disabled", "reason", err)
	} else {
		llmClient = c
	}

	api := httpapi.New(st, llmClient)
	// Consolidate on its own once uploads go quiet, so a remote collector
	// feeding this server keeps memory current with nobody poking an endpoint.
	if after := envDuration("LARD_CONSOLIDATE_AFTER", httpapi.DefaultConsolidateAfter); after > 0 {
		api.EnableAutoConsolidate(after, envDuration("LARD_CONSOLIDATE_MAX_WAIT", httpapi.DefaultConsolidateMaxWait))
		defer api.StopAutoConsolidate()
	}
	mcpSrv := mcpserver.New(api)

	cfg := auth.Config{
		Mode:              auth.Mode(envOr("LARD_AUTH", string(auth.ModeNone))),
		Token:             os.Getenv("LARD_TOKEN"),
		IndikoURL:         envOr("LARD_INDIKO_URL", "https://indiko.dunkirk.sh"),
		PublicURL:         os.Getenv("LARD_PUBLIC_URL"),
		AllowedClientIDs:  envList("LARD_OAUTH_CLIENT_IDS"),
		AllowedUsers:      envList("LARD_OAUTH_USERS"),
		RequiredScopes:    envList("LARD_OAUTH_SCOPES"),
		CollectorClientID: os.Getenv("LARD_COLLECTOR_CLIENT_ID"),
	}

	// The collector registration: which OAuth client edge collectors adopt.
	// Login itself is the device grant against the authorization server, so
	// this server only publishes the identity.
	collectorCfg := collector.Config{
		ClientID: os.Getenv("LARD_COLLECTOR_CLIENT_ID"),
		Scopes:   envList("LARD_COLLECTOR_SCOPES"),
	}
	collectorH := collector.New(collectorCfg)
	if collectorCfg.Configured() {
		slog.Info("collector registration published", "client_id", collectorCfg.ClientID)
	}
	for _, warn := range cfg.Validate() {
		slog.Warn("auth: " + warn)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTPHandler(mcpSrv))
	// The trailing slash covers RFC 9728's path-suffixed documents (e.g.
	// /.well-known/oauth-protected-resource/mcp), which is what MCP clients
	// actually request.
	mux.Handle(auth.PathProtectedResource, auth.ProtectedResourceMetadata(cfg))
	mux.Handle(auth.PathProtectedResource+"/", auth.ProtectedResourceMetadata(cfg))
	mux.Handle(auth.PathAuthServer, auth.AuthServerMetadata(cfg))
	mux.HandleFunc("GET "+auth.PathCollector, collectorH.Register)
	mux.Handle("/", api.Handler())

	addr := envOr("LARD_ADDR", ":7477")
	srv := &http.Server{
		Addr:              addr,
		Handler:           auth.Middleware(cfg, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("lard listening", "addr", addr, "db", dbPath, "memory", memDir, "auth", cfg.Mode, "mcp", addr+"/mcp")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envList reads a comma-separated env var into a trimmed, non-empty slice.
func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envDuration reads a Go duration (e.g. "5m"). "off" or "0" disables the
// feature by returning zero; an unparseable value falls back to the default.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if raw == "off" || raw == "never" || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("ignoring unparseable duration", "key", key, "value", raw)
		return fallback
	}
	return d
}

func defaultDBPath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "lard", "lard.db")
	}
	return "lard.db"
}

func defaultMemDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "lard", "memory")
	}
	return "memory"
}
