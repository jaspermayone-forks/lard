// lard-client is the edge collector. Two modes:
//
//	lard-client backfill --root ~/code --root ~/code/charm
//	    Scan for every .crush/crush.db under the roots and upload all
//	    sessions ever. Idempotent; safe to re-run.
//
//	lard-client daemon [--workspace .] [--interval 5m]
//	    Periodically collect new/changed sessions and upload. Run per
//	    machine; with --workspace it watches a single repo, otherwise it
//	    rescans the roots each tick.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/taciturnaxolotl/lard/internal/client"
	"github.com/taciturnaxolotl/lard/internal/dotenv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lard-client:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dotenv.LoadDefault()
	if len(args) == 0 {
		usage()
		return fmt.Errorf("subcommand required: backfill | daemon | sync")
	}
	switch args[0] {
	case "backfill":
		return runSync(args[1:], true, false)
	case "sync":
		return runSync(args[1:], false, false)
	case "daemon":
		return runDaemon(args[1:])
	case "consolidate":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return uploader().Consolidate(ctx)
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  lard-client backfill --root DIR [--root DIR...] [--dry-run]
  lard-client sync     [--workspace DIR...]
  lard-client daemon   [--workspace DIR...] [--root DIR...] [--interval 5m]

env:
  LARD_URL    central service base URL (default http://localhost:7477)
  LARD_TOKEN  bearer token if the service requires auth
`)
}

func uploader() *client.Uploader {
	base := os.Getenv("LARD_URL")
	if base == "" {
		base = "http://localhost:7477"
	}
	return client.NewUploader(base, os.Getenv("LARD_TOKEN"))
}

type rootFlags []string

func (r *rootFlags) String() string { return fmt.Sprint([]string(*r)) }
func (r *rootFlags) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func runSync(args []string, full, _ bool) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var roots, workspaces rootFlags
	var dryRun bool
	fs.Var(&roots, "root", "root to scan for .crush dirs (repeatable)")
	fs.Var(&workspaces, "workspace", "explicit workspace with .crush (repeatable)")
	fs.BoolVar(&dryRun, "dry-run", false, "collect but do not upload")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 && len(workspaces) == 0 {
		home, _ := os.UserHomeDir()
		roots = rootFlags{home + "/code"}
	}
	return doSync(full, dryRun, roots, workspaces)
}

func doSync(full, dryRun bool, roots, workspaces []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := client.LoadState(client.DefaultStatePath())
	if err != nil {
		return err
	}
	return client.Sync(ctx, uploader(), st, client.SyncOpts{
		Workspaces: workspaces,
		Roots:      roots,
		Full:       full,
		Collector:  client.Hostname(),
		DryRun:     dryRun,
	})
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	var roots, workspaces rootFlags
	var interval time.Duration
	fs.Var(&roots, "root", "root to scan for .crush dirs (repeatable)")
	fs.Var(&workspaces, "workspace", "explicit workspace with .crush (repeatable)")
	fs.DurationVar(&interval, "interval", 5*time.Minute, "sync interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 && len(workspaces) == 0 {
		home, _ := os.UserHomeDir()
		roots = rootFlags{home + "/code"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("lard-client daemon", "interval", interval, "roots", roots, "workspaces", workspaces)
	// Tick immediately, then on interval.
	tick := func() {
		if err := doSync(false, false, roots, workspaces); err != nil {
			slog.Error("sync", "error", err)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			tick()
		}
	}
}
