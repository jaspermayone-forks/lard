// Package client is the edge collector: it reads Crush session databases,
// normalizes user turns into the common wire schema, and uploads them to
// the central lard service. No LLM work happens here; this is dumb
// transport with an idempotent upload path.
package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// CrushDB is a read-only handle on one workspace's .crush/crush.db.
type CrushDB struct {
	Workspace string
	DBPath    string
}

// OpenCrush opens the crush database for a workspace read-only.
// It tolerates Crush holding a write lock by never writing.
func OpenCrush(workspace string) (*sql.DB, string, error) {
	dbPath := filepath.Join(workspace, ".crush", "crush.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, dbPath, err
	}
	params := url.Values{}
	params.Set("mode", "ro")
	params.Set("_pragma", "busy_timeout(5000)")
	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, dbPath, err
	}
	db.SetMaxOpenConns(1)
	return db, dbPath, nil
}

// SessionSince lists sessions whose updated_at exceeds the watermark
// (unix seconds). Watermark 0 backfills everything.
func SessionSince(ctx context.Context, db *sql.DB, since int64) ([]crushSession, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, created_at, updated_at FROM sessions
		 WHERE updated_at > ? AND parent_session_id IS NULL
		 ORDER BY created_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []crushSession
	for rows.Next() {
		var s crushSession
		if err := rows.Scan(&s.id, &s.createdAt, &s.updatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type crushSession struct {
	id               string
	createdAt, updatedAt int64
}

// userTurns reads a session's user messages, ordered, and flattens them to
// text. Assistant and tool messages never leave the machine.
func userTurns(ctx context.Context, db *sql.DB, sessionID string) ([]types.Turn, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT parts, created_at FROM messages
		 WHERE session_id = ? AND role = 'user' ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.Turn
	i := 0
	for rows.Next() {
		var parts string
		var createdAt int64
		if err := rows.Scan(&parts, &createdAt); err != nil {
			return nil, err
		}
		content := flattenParts(parts)
		if strings.TrimSpace(content) == "" {
			continue
		}
		out = append(out, types.Turn{
			Index:   i,
			Role:    "user",
			Content: content,
			TS:      time.Unix(createdAt, 0).UTC().Format(time.RFC3339),
		})
		i++
	}
	return out, rows.Err()
}

// flattenParts extracts text from a crush message's parts JSON. Crush user
// messages are usually plain text; attachments keep their text and drop
// binary scaffolding.
func flattenParts(raw string) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		// Not JSON: treat the whole blob as text.
		return raw
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "file":
			// Attachments carry a text extraction in Data for text files.
			if p.Data != "" && len(p.Data) < 100_000 {
				b.WriteString("\n")
				b.WriteString(p.Data)
			}
		}
	}
	return b.String()
}

// ProjectHints computes identity signals for a workspace (§4.1): the
// normalized git remote (strongest, portable) and the path itself (weakest).
func ProjectHints(workspace string) *types.ProjectHints {
	h := &types.ProjectHints{Path: workspace}
	if out, err := exec.Command("git", "-C", workspace, "remote", "get-url", "origin").Output(); err == nil {
		h.GitRemote = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel").Output(); err == nil {
		h.Path = strings.TrimSpace(string(out))
	}
	return h
}

// CollectBatch reads all sessions newer than the watermark from one
// workspace's crush DB and returns them as SessionBatches, plus the new
// watermark to persist after a successful upload.
func CollectBatch(ctx context.Context, workspace string, since int64) (sessions []types.SessionBatch, newWatermark int64, err error) {
	db, _, err := OpenCrush(workspace)
	if err != nil {
		return nil, since, err
	}
	defer db.Close()

	hints := ProjectHints(workspace)
	rows, err := SessionSince(ctx, db, since)
	if err != nil {
		return nil, since, fmt.Errorf("list sessions: %w", err)
	}
	newWatermark = since
	for _, s := range rows {
		turns, err := userTurns(ctx, db, s.id)
		if err != nil {
			return nil, since, fmt.Errorf("session %s: %w", s.id, err)
		}
		if len(turns) == 0 {
			continue
		}
		sessions = append(sessions, types.SessionBatch{
			SessionID:    s.id,
			Source:       "crush",
			ProjectHints: hints,
			StartedAt:    time.Unix(s.createdAt, 0).UTC().Format(time.RFC3339),
			EndedAt:      time.Unix(s.updatedAt, 0).UTC().Format(time.RFC3339),
			Turns:        turns,
		})
		if s.updatedAt > newWatermark {
			newWatermark = s.updatedAt
		}
	}
	return sessions, newWatermark, nil
}

// FindCrushWorkspaces walks root looking for directories that contain
// .crush/crush.db. It skips hidden dirs and common dependency folders.
func FindCrushWorkspaces(root string) ([]string, error) {
	var out []string
	skip := map[string]bool{
		"node_modules": true, ".git": true, ".hg": true, ".svn": true,
		"vendor": true, ".cache": true, ".npm": true, ".cargo": true,
		".rustup": true, "go": true, "Library": true, ".Trash": true,
	}
	root = filepath.Clean(root)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate permission errors
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (skip[name] || strings.HasPrefix(name, ".")) && name != ".crush" {
			return filepath.SkipDir
		}
		if name == ".crush" {
			if _, err := os.Stat(filepath.Join(path, "crush.db")); err == nil {
				out = append(out, filepath.Dir(path))
			}
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}
