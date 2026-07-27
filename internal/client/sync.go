package client

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// SyncOpts controls a sync run.
type SyncOpts struct {
	Workspaces []string // explicit workspaces; if empty, scan Roots
	Roots      []string // roots to scan for .crush dirs (backfill)
	Full       bool     // ignore watermarks; re-send everything
	Collector  string   // collector id, e.g. hostname
	DryRun     bool
}

// Sync collects sessions from every known workspace and uploads them.
// Idempotent: server-side (source, sessionID) upsert absorbs overlaps, and
// watermarks only advance after a successful upload.
func Sync(ctx context.Context, up *Uploader, st *State, opts SyncOpts) error {
	workspaces := opts.Workspaces
	if len(workspaces) == 0 {
		for _, root := range opts.Roots {
			found, err := FindCrushWorkspaces(root)
			if err != nil {
				slog.Warn("scan root", "root", root, "error", err)
				continue
			}
			workspaces = append(workspaces, found...)
		}
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no crush workspaces found")
	}
	slog.Info("syncing workspaces", "count", len(workspaces))

	const maxBatch = 50 // sessions per POST
	for _, ws := range workspaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		since := st.Watermarks[ws]
		if opts.Full {
			since = 0
		}
		sessions, newWatermark, err := CollectBatch(ctx, ws, since)
		if err != nil {
			slog.Warn("collect", "workspace", ws, "error", err)
			continue
		}
		if len(sessions) == 0 {
			continue
		}
		slog.Info("collected", "workspace", ws, "sessions", len(sessions))
		if opts.DryRun {
			continue
		}
		for i := 0; i < len(sessions); i += maxBatch {
			end := i + maxBatch
			if end > len(sessions) {
				end = len(sessions)
			}
			req := types.IngestRequest{Collector: opts.Collector, Sessions: sessions[i:end]}
			if err := up.Ingest(ctx, req); err != nil {
				return fmt.Errorf("ingest %s: %w", ws, err)
			}
		}
		st.Watermarks[ws] = newWatermark
		if err := st.Save(); err != nil {
			slog.Warn("save state", "error", err)
		}
	}
	return nil
}

// Hostname is the default collector id.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return strings.TrimSpace(h)
}
