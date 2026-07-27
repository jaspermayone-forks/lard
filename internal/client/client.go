package client

import (
	"bytes"
	"context"
	"encoding/json"
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

// Consolidate triggers a server-side consolidation pass.
func (u *Uploader) Consolidate(ctx context.Context) error {
	return u.post(ctx, "/consolidate", nil, nil)
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
