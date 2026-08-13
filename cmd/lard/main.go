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
	"github.com/taciturnaxolotl/lard/internal/backup"
	"github.com/taciturnaxolotl/lard/internal/config"
	"github.com/taciturnaxolotl/lard/internal/dotenv"
	"github.com/taciturnaxolotl/lard/internal/httpapi"
	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/mcpserver"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/tenant"
)

const usage = `lard is the central memory service.

  lard                     run the server
  lard backup <dir>        copy every store into <dir>, live, without stopping
  lard restore <dir> [-f]  put a backup tree back where the server reads it
`

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "backup":
		err = runBackup(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "restore":
		err = runRestore(os.Args[2:])
	case len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help"):
		fmt.Print(usage)
		return
	case len(os.Args) > 1:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)
	default:
		err = run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lard:", err)
		os.Exit(1)
	}
}

// loadConfig reads the server's configuration the same way for every command,
// so a backup can never disagree with the running server about where the data
// is.
func loadConfig() (*config.Server, error) {
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
		return nil, fmt.Errorf("load config: %w", err)
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
	return cfg, nil
}

// runBackup copies every store into a destination directory, laid out exactly
// like the live one. Nothing is stopped: databases are snapshotted inside a
// transaction and subject files are written by rename, so a backup tool
// pointed at the destination sees a still directory.
func runBackup(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: lard backup <dir>")
	}
	dest := args[0]
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, err := backup.Run(dest, backup.Options{
		MultiUser: cfg.MultiUser,
		DB:        cfg.DB,
		MemoryDir: cfg.MemoryDir,
		DataDir:   cfg.DataDir,
	})
	if err != nil {
		return err
	}
	fmt.Printf("backed up %d store(s) to %s (%.1f MiB)\n", len(res.Sources), dest, float64(res.Bytes)/(1<<20))
	for _, name := range res.Sources {
		fmt.Println("  " + name)
	}
	return nil
}

// runRestore puts a backup tree back where the server reads it. Stop the
// server first: this rewrites the files underneath it.
func runRestore(args []string) error {
	var src string
	var force bool
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		default:
			if src != "" {
				return fmt.Errorf("usage: lard restore <dir> [--force]")
			}
			src = a
		}
	}
	if src == "" {
		return fmt.Errorf("usage: lard restore <dir> [--force]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, err := backup.Restore(src, backup.Options{
		MultiUser: cfg.MultiUser,
		DB:        cfg.DB,
		MemoryDir: cfg.MemoryDir,
		DataDir:   cfg.DataDir,
	}, force)
	if err != nil {
		return err
	}
	for _, aside := range res.MovedAside {
		fmt.Println("kept previous data at " + aside)
	}
	fmt.Printf("restored %d path(s) from %s (%.1f MiB)\n", len(res.Sources), src, float64(res.Bytes)/(1<<20))
	for _, dst := range res.Sources {
		fmt.Println("  " + dst)
	}
	return nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
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
			// Empty counts as absent. A backup hook running
			// "sqlite3 <db> PRAGMA wal_checkpoint" against a path that moved
			// leaves a 0-byte file behind, and that file must not be mistaken
			// for a database worth serving.
			if info, err := os.Stat(cfg.DB); err != nil || info.Size() == 0 {
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
		Mode:           auth.Mode(cfg.Auth.Mode),
		Token:          cfg.Auth.Token,
		AuthServerURL:  cfg.Auth.AuthServerURL,
		PublicURL:      cfg.Auth.PublicURL,
		AllowedUsers:   cfg.Auth.AllowedUsers,
		RequiredScopes: cfg.Auth.RequiredScopes,
		ResourceName:   cfg.Auth.ResourceName,
		LogoURI:        cfg.Auth.LogoURI,
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
