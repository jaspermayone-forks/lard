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
	"syscall"
	"time"

	"github.com/taciturnaxolotl/lard/internal/auth"
	"github.com/taciturnaxolotl/lard/internal/collector"
	"github.com/taciturnaxolotl/lard/internal/config"
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

	// Load configuration from TOML file (if present) with env var overrides.
	configPath := os.Getenv("LARD_CONFIG")
	if configPath == "" {
		if d, err := os.UserConfigDir(); err == nil {
			configPath = filepath.Join(d, "lard", "config.toml")
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Fill in defaults for paths if not set.
	if cfg.DB == "" {
		cfg.DB = defaultDBPath()
	}
	if cfg.MemoryDir == "" {
		cfg.MemoryDir = defaultMemDir()
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DB, cfg.MemoryDir)
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
	if after := cfg.Consolidate.ConsolidateAfter(); after > 0 {
		api.EnableAutoConsolidate(after, cfg.Consolidate.ConsolidateMaxWait())
		defer api.StopAutoConsolidate()
	}
	mcpSrv := mcpserver.New(api)

	authCfg := auth.Config{
		Mode:              auth.Mode(cfg.Auth.Mode),
		Token:             cfg.Auth.Token,
		IndikoURL:         cfg.Auth.IndikoURL,
		PublicURL:         cfg.Auth.PublicURL,
		AllowedClientIDs:  cfg.Auth.AllowedClientIDs,
		AllowedUsers:      cfg.Auth.AllowedUsers,
		RequiredScopes:    cfg.Auth.RequiredScopes,
		CollectorClientID: cfg.Auth.CollectorClientID,
	}

	// The collector registration: which OAuth client edge collectors adopt.
	// Login itself is the device grant against the authorization server, so
	// this server only publishes the identity.
	collectorCfg := collector.Config{
		ClientID: cfg.Collector.ClientID,
		Scopes:   cfg.Collector.Scopes,
	}
	collectorH := collector.New(collectorCfg)
	if collectorCfg.Configured() {
		slog.Info("collector registration published", "client_id", collectorCfg.ClientID)
	}
	for _, warn := range authCfg.Validate() {
		slog.Warn("auth: " + warn)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTPHandler(mcpSrv))
	// The trailing slash covers RFC 9728's path-suffixed documents (e.g.
	// /.well-known/oauth-protected-resource/mcp), which is what MCP clients
	// actually request.
	mux.Handle(auth.PathProtectedResource, auth.ProtectedResourceMetadata(authCfg))
	mux.Handle(auth.PathProtectedResource+"/", auth.ProtectedResourceMetadata(authCfg))
	mux.Handle(auth.PathAuthServer, auth.AuthServerMetadata(authCfg))
	mux.HandleFunc("GET "+auth.PathCollector, collectorH.Register)
	mux.Handle("/", api.Handler())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           auth.Middleware(authCfg, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("lard listening", "addr", cfg.Addr, "db", cfg.DB, "memory", cfg.MemoryDir, "auth", authCfg.Mode, "mcp", cfg.Addr+"/mcp")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
