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
	"github.com/taciturnaxolotl/lard/internal/tenant"
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
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}

	// Multi-user memory is keyed by the identity on the token, and only oauth
	// mode produces one. Without oauth every request would land on
	// primary_user, so a missing primary_user means nothing can be served at
	// all: say so at boot rather than 401ing every request.
	if cfg.MultiUser && auth.Mode(cfg.Auth.Mode) != auth.ModeOAuth {
		if cfg.PrimaryUser == "" {
			return fmt.Errorf("multi_user needs auth.mode = \"oauth\" (or a primary_user to attribute unidentified requests to)")
		}
		slog.Warn("multi_user without oauth: every caller shares one memory", "primary_user", cfg.PrimaryUser)
	}

	// The LLM client is optional at boot: the API works without it, only
	// /consolidate refuses.
	var llmClient *llm.Client
	if c, err := llm.New(ctx, cfg.LLM); err != nil {
		slog.Warn("no LLM client; /consolidate disabled", "reason", err)
	} else {
		llmClient = c
	}

	var api *httpapi.Server
	if cfg.MultiUser {
		layout := tenant.Layout{Root: cfg.DataDir}
		if err := os.MkdirAll(layout.Root, 0o700); err != nil {
			return err
		}
		// A server that was single-user until this boot has a database full of
		// memory the owner would otherwise silently lose. Move it in.
		if cfg.PrimaryUser != "" {
			slug := tenant.Slug(cfg.PrimaryUser)
			moved, err := tenant.AdoptLegacy(layout, slug, cfg.DB, cfg.MemoryDir)
			if err != nil {
				return fmt.Errorf("adopt single-user data: %w", err)
			}
			if moved {
				slog.Info("adopted single-user data", "user", cfg.PrimaryUser, "dir", layout.Dir(slug))
			}
		}
		api = httpapi.NewMultiUser(httpapi.MultiUserConfig{
			Layout:      layout,
			PrimaryUser: cfg.PrimaryUser,
		}, llmClient)
	} else {
		// Turning multi_user back off is the one way to lose sight of your
		// memory: the tenant directories still hold it, but this path would
		// open a brand new empty database and say nothing. Refuse instead, and
		// name the directory the data is actually in.
		if tenants := tenant.List(tenant.Layout{Root: cfg.DataDir}); len(tenants) > 0 {
			if _, err := os.Stat(cfg.DB); err != nil {
				return fmt.Errorf("multi_user is off but %d tenant(s) hold the memory under %s; "+
					"turn multi_user back on, or move %s/{lard.db,memory} to %s and %s",
					len(tenants), cfg.DataDir, filepath.Join(cfg.DataDir, tenants[0]), cfg.DB, cfg.MemoryDir)
			}
			slog.Warn("multi_user is off; tenant memory under data_dir is not being served", "tenants", len(tenants), "data_dir", cfg.DataDir)
		}
		if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o700); err != nil {
			return err
		}
		st, err := store.Open(cfg.DB, cfg.MemoryDir)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		api = httpapi.New(st, llmClient)
	}
	defer api.Close()
	// Consolidate on its own once uploads go quiet, so a remote collector
	// feeding this server keeps memory current with nobody poking an endpoint.
	if after := cfg.Consolidate.ConsolidateAfter(); after > 0 {
		api.EnableAutoConsolidate(after, cfg.Consolidate.ConsolidateMaxWait())
		defer api.StopAutoConsolidate()
	}

	authCfg := auth.Config{
		Mode:              auth.Mode(cfg.Auth.Mode),
		Token:             cfg.Auth.Token,
		AuthServerURL:     cfg.Auth.AuthServerURL,
		PublicURL:         cfg.Auth.PublicURL,
		AllowedClientIDs:  cfg.Auth.AllowedClientIDs,
		AllowedUsers:      cfg.Auth.AllowedUsers,
		RequiredScopes:    cfg.Auth.RequiredScopes,
		CollectorClientID: cfg.Collector.ClientID,
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
	mux.Handle("/mcp", mcpserver.HTTPHandler(api))
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

	if cfg.MultiUser {
		slog.Info("lard listening", "addr", cfg.Addr, "data", cfg.DataDir, "users", "multi", "auth", authCfg.Mode, "mcp", cfg.Addr+"/mcp")
	} else {
		slog.Info("lard listening", "addr", cfg.Addr, "db", cfg.DB, "memory", cfg.MemoryDir, "auth", authCfg.Mode, "mcp", cfg.Addr+"/mcp")
	}
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

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "lard", "users")
	}
	return "users"
}
