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

	dbPath := envOr("LARD_DB", defaultDBPath())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
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
	mcpSrv := mcpserver.New(api)

	cfg := auth.Config{
		Mode:      auth.Mode(envOr("LARD_AUTH", string(auth.ModeNone))),
		Token:     os.Getenv("LARD_TOKEN"),
		IndikoURL: envOr("LARD_INDIKO_URL", "https://indiko.dunkirk.sh"),
		PublicURL: os.Getenv("LARD_PUBLIC_URL"),
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTPHandler(mcpSrv))
	mux.Handle("/.well-known/oauth-authorization-server", auth.WellKnownOAuthMetadata(cfg))
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

	slog.Info("lard listening", "addr", addr, "db", dbPath, "auth", cfg.Mode, "mcp", addr+"/mcp")
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

func defaultDBPath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "lard", "lard.db")
	}
	return "lard.db"
}
