// lard-client is the edge collector: it finds Crush session databases on this
// machine and uploads them to a central lard.
//
// First run asks where the server lives and walks a browser login, so setup is
// `lard-client login` with nothing else to look up. Every prompt has a flag
// equivalent, for scripts and headless machines.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

	"github.com/taciturnaxolotl/lard/internal/client"
	"github.com/taciturnaxolotl/lard/internal/dotenv"
	"github.com/taciturnaxolotl/lard/internal/service"
	"github.com/taciturnaxolotl/lard/internal/setup"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	dotenv.LoadDefault()
	if err := fang.Execute(context.Background(), rootCmd(), fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "lard-client",
		Short: "Collect Crush sessions and send them to lard",
		Long: `lard-client finds Crush session databases on this machine and uploads
them to a central lard server, which turns them into durable memory.

Start with 'lard-client login', then 'lard-client backfill'.`,
		SilenceUsage: true,
	}
	root.AddCommand(
		loginCmd(),
		backfillCmd(),
		syncCmd(),
		daemonCmd(),
		consolidateCmd(),
		serviceCmd(),
		statusCmd(),
	)
	return root
}

// --- login ---

func loginCmd() *cobra.Command {
	var opts setup.Options
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Connect this machine to a lard server",
		Long: `Point the collector at a server and get credentials.

With no flags this asks for the server URL, then opens your browser to
authorize. On a headless machine pass --url and --token instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := setup.Run(cmd.Context(), opts)
			if err != nil {
				return err
			}
			printConnected(cmd.Context(), cfg, opts.CallbackPort)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.URL, "url", "", "server base URL (asked for if omitted)")
	f.StringVar(&opts.Token, "token", "", "shared secret instead of the browser login")
	f.StringSliceVar(&opts.Roots, "root", nil, "directory to scan for Crush sessions (repeatable)")
	f.IntVar(&opts.CallbackPort, "port", client.CallbackPort, "localhost port for the OAuth callback")
	f.BoolVar(&opts.NoBrowser, "no-browser", false, "print the authorization URL instead of opening it")
	return cmd
}

func printConnected(ctx context.Context, cfg *client.Config, port int) {
	fmt.Printf("Connected to %s via %s.\n", cfg.URL, cfg.AuthMode())
	// Only mention the client id when we had to invent one, since that is the
	// case where the operator must add it to the server's allowlist. A
	// server-published id is trusted by definition.
	if cfg.AuthMode() == "oauth" {
		if _, err := client.FetchRegistration(ctx, cfg.URL); err != nil {
			if port <= 0 {
				port = client.CallbackPort
			}
			fmt.Printf("OAuth client id: %s\n", client.ClientID(port))
			fmt.Println("  This server publishes no collector registration, so add that id to")
			fmt.Println("  its LARD_OAUTH_CLIENT_IDS, or set LARD_COLLECTOR_CLIENT_ID there instead.")
		}
	}
	fmt.Printf("Saved %s\n\nNext: lard-client backfill\n", client.DefaultConfigPath())
}

// --- collection ---

func backfillCmd() *cobra.Command {
	var roots []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Upload every session ever recorded",
		Long: `Scan for Crush databases and upload all sessions, not just new ones.

Idempotent: safe to run again, and safe to interrupt.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doSync(cmd.Context(), true, dryRun, configuredRoots(roots), nil)
		},
	}
	cmd.Flags().StringSliceVar(&roots, "root", nil, "directory to scan (repeatable; defaults to the saved config)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "collect but do not upload")
	return cmd
}

func syncCmd() *cobra.Command {
	var roots, workspaces []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Upload sessions changed since the last run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := roots
			if len(workspaces) == 0 {
				r = configuredRoots(roots)
			}
			return doSync(cmd.Context(), false, dryRun, r, workspaces)
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&roots, "root", nil, "directory to scan (repeatable)")
	f.StringSliceVar(&workspaces, "workspace", nil, "single repo to sync (repeatable)")
	f.BoolVar(&dryRun, "dry-run", false, "collect but do not upload")
	return cmd
}

func daemonCmd() *cobra.Command {
	var roots, workspaces []string
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Sync on an interval in the foreground",
		Long: `Sync repeatedly until interrupted.

On macOS prefer 'lard-client service install', which lets launchd own the
schedule. Use this under systemd or in a container.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := roots
			if len(workspaces) == 0 {
				r = configuredRoots(roots)
			}
			slog.Info("lard-client daemon", "interval", interval, "roots", r, "workspaces", workspaces)
			tick := func() {
				if err := doSync(cmd.Context(), false, false, r, workspaces); err != nil {
					slog.Error("sync", "error", err)
				}
			}
			tick()
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-t.C:
					tick()
				}
			}
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&roots, "root", nil, "directory to scan (repeatable)")
	f.StringSliceVar(&workspaces, "workspace", nil, "single repo to sync (repeatable)")
	f.DurationVar(&interval, "interval", 5*time.Minute, "sync interval")
	return cmd
}

func consolidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate",
		Short: "Ask the server to consolidate now",
		Long: `Force a consolidation pass.

The server normally does this on its own once uploads go quiet, so this is
only needed to skip the wait.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			up, err := uploader(cmd.Context())
			if err != nil {
				return err
			}
			return up.Consolidate(cmd.Context())
		},
	}
}

// --- status ---

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the configured server and background agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := client.LoadConfig(client.DefaultConfigPath())
			if err != nil {
				return err
			}
			fmt.Printf("server:  %s\n", cfg.URL)
			fmt.Printf("auth:    %s\n", cfg.AuthMode())
			fmt.Printf("roots:   %s\n", strings.Join(configuredRoots(nil), ", "))
			if id, err := cfg.Verify(cmd.Context()); err != nil {
				fmt.Printf("reach:   %v\n", err)
			} else if id != "" {
				fmt.Printf("reach:   ok, as %s\n", id)
			} else {
				fmt.Println("reach:   ok")
			}
			if service.Supported() {
				installed, loaded, detail, serr := service.Status()
				switch {
				case serr != nil:
					fmt.Printf("agent:   %v\n", serr)
				case !installed:
					fmt.Println("agent:   not installed (lard-client service install)")
				case !loaded:
					fmt.Println("agent:   installed but not loaded")
				default:
					fmt.Printf("agent:   %s\n", detail)
				}
			}
			return nil
		},
	}
}

// --- service ---

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background sync agent",
	}
	var roots []string
	var interval time.Duration
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background agent",
		Long: `Install a launchd agent that syncs on an interval and survives reboots.

Refuses to install if the server cannot be reached, since a background agent
that fails silently is worse than none.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := client.LoadConfig(client.DefaultConfigPath())
			if err != nil {
				return err
			}
			if _, err := cfg.Verify(cmd.Context()); err != nil {
				return fmt.Errorf("%w\nrun 'lard-client login' first", err)
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			if bin, err = filepath.EvalSymlinks(bin); err != nil {
				return err
			}
			r := configuredRoots(roots)
			path, err := service.Install(service.Options{Binary: bin, Interval: interval, Roots: r})
			if err != nil {
				return err
			}
			fmt.Printf("Installed %s\nSyncing every %s from %s\nLogs: ~/Library/Logs/lard-client.log\n",
				path, interval, strings.Join(r, ", "))
			return nil
		},
	}
	install.Flags().StringSliceVar(&roots, "root", nil, "directory to scan (repeatable; defaults to the saved config)")
	install.Flags().DurationVar(&interval, "interval", 5*time.Minute, "sync interval")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background agent",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := service.Uninstall(); err != nil {
				return err
			}
			fmt.Println("Uninstalled.")
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Report whether the agent is running",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			installed, loaded, detail, err := service.Status()
			if err != nil {
				return err
			}
			switch {
			case !installed:
				fmt.Println("Not installed; run 'lard-client service install'.")
			case !loaded:
				fmt.Println("Installed but not loaded; run 'lard-client service install' again.")
			default:
				fmt.Println(detail)
			}
			return nil
		},
	}
	cmd.AddCommand(install, uninstall, status)
	return cmd
}

// --- shared ---

// uploader builds an Uploader from the saved config, refreshing an expired
// OAuth token first so a background run never fails on a stale credential.
func uploader(ctx context.Context) (*client.Uploader, error) {
	path := client.DefaultConfigPath()
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	tok, err := cfg.Bearer(ctx, path)
	if err != nil {
		return nil, err
	}
	return client.NewUploader(cfg.URL, tok), nil
}

// configuredRoots resolves which directories to scan: explicit flags, then the
// saved config, then ~/code.
func configuredRoots(flagRoots []string) []string {
	if len(flagRoots) > 0 {
		return flagRoots
	}
	if cfg, err := client.LoadConfig(client.DefaultConfigPath()); err == nil && len(cfg.Roots) > 0 {
		return cfg.Roots
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, "code")}
}

func doSync(ctx context.Context, full, dryRun bool, roots, workspaces []string) error {
	st, err := client.LoadState(client.DefaultStatePath())
	if err != nil {
		return err
	}
	up, err := uploader(ctx)
	if err != nil {
		return err
	}
	return client.Sync(ctx, up, st, client.SyncOpts{
		Workspaces: workspaces,
		Roots:      roots,
		Full:       full,
		Collector:  client.Hostname(),
		DryRun:     dryRun,
	})
}
