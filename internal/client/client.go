package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/taciturnaxolotl/lard/internal/types"
	"github.com/taciturnaxolotl/lard/internal/xdg"
)

// Uploader pushes session batches to the central lard service.
type Uploader struct {
	BaseURL string
	Token   string // bearer token (LARD_TOKEN or OAuth access token)
	HTTP    *http.Client
}

// NewUploader builds an uploader for a base URL like https://lard.lan:7477.
func NewUploader(baseURL, token string) *Uploader {
	return &Uploader{
		BaseURL: baseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// post sends one JSON request and decodes the JSON reply. A nil body means
// no payload; a nil out means the reply body is ignored. Non-2xx responses
// come back as an error carrying the server's message.
func (u *Uploader) post(ctx context.Context, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+path, reader)
	if err != nil {
		return err
	}
	r.Header.Set("content-type", "application/json")
	if u.Token != "" {
		r.Header.Set("authorization", "Bearer "+u.Token)
	}
	resp, err := u.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %d: %s", path, resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ingest posts an IngestRequest to /ingest.
func (u *Uploader) Ingest(ctx context.Context, req types.IngestRequest) error {
	return u.post(ctx, "/ingest", req, nil)
}

// ConsolidateResult reports what the server's consolidation pass did.
type ConsolidateResult struct {
	Extracted   int `json:"extracted"`   // sessions that yielded facts
	Synthesized int `json:"synthesized"` // subject files rewritten
}

// ProgressFn gets one call per completed step of a consolidation pass.
type ProgressFn func(phase, name string, done, total int)

// Consolidate triggers a server-side consolidation pass and waits for it,
// streaming progress as steps finish and reporting what the pass did. A
// backfill can take a long time (one LLM call per session and per dirty
// subject), so the request itself carries no timeout; cancellation comes
// from ctx. The pass survives a client going away: it is detached on the
// server side.
func (u *Uploader) Consolidate(ctx context.Context, progress ProgressFn) (*ConsolidateResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+"/consolidate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/x-ndjson")
	if u.Token != "" {
		req.Header.Set("authorization", "Bearer "+u.Token)
	}
	cl := *u.HTTP
	cl.Timeout = 0 // let ctx own cancellation for a long-running pass
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("/consolidate %d: %s", resp.StatusCode, string(b))
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Phase       string `json:"phase"`
			Name        string `json:"name"`
			Done        int    `json:"done"`
			Total       int    `json:"total"`
			Finished    bool   `json:"finished"`
			Extracted   int    `json:"extracted"`
			Synthesized int    `json:"synthesized"`
			Error       string `json:"error"`
		}
		if err := dec.Decode(&ev); err != nil {
			return nil, fmt.Errorf("consolidate stream ended early: %w", err)
		}
		if ev.Finished {
			if ev.Error != "" {
				return nil, errors.New(ev.Error)
			}
			return &ConsolidateResult{Extracted: ev.Extracted, Synthesized: ev.Synthesized}, nil
		}
		if progress != nil {
			progress(ev.Phase, ev.Name, ev.Done, ev.Total)
		}
	}
}

// ResolveProject asks the service to canonicalize hints and returns the id.
func (u *Uploader) ResolveProject(ctx context.Context, hints *types.ProjectHints) (string, error) {
	var out struct {
		ProjectID string `json:"projectId"`
	}
	if err := u.post(ctx, "/projects/resolve", map[string]any{"hints": hints}, &out); err != nil {
		return "", err
	}
	return out.ProjectID, nil
}

// State persists per-workspace watermarks between runs.
type State struct {
	path       string
	Watermarks map[string]int64 `json:"watermarks"` // workspace → max session updated_at uploaded
}

// LoadState reads the state file, tolerating absence.
func LoadState(path string) (*State, error) {
	s := &State{path: path, Watermarks: map[string]int64{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	s.path = path
	if s.Watermarks == nil {
		s.Watermarks = map[string]int64{}
	}
	return s, nil
}

// Save writes state atomically.
func (s *State) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// DefaultStatePath is ~/.local/share/lard/client-state.json. State, not
// config: it is a sync watermark the user never edits by hand.
func DefaultStatePath() string {
	return xdg.DataPath("client-state.json")
}
