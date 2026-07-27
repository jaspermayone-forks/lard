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

// Ingest posts an IngestRequest to /ingest.
func (u *Uploader) Ingest(ctx context.Context, req types.IngestRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+"/ingest", bytes.NewReader(body))
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
		return fmt.Errorf("ingest %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// Consolidate triggers a server-side consolidation pass.
func (u *Uploader) Consolidate(ctx context.Context) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+"/consolidate", nil)
	if err != nil {
		return err
	}
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
		return fmt.Errorf("consolidate %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ResolveProject asks the service to canonicalize hints and returns the id.
func (u *Uploader) ResolveProject(ctx context.Context, hints *types.ProjectHints) (string, error) {
	body, _ := json.Marshal(map[string]any{"hints": hints})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+"/projects/resolve", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	r.Header.Set("content-type", "application/json")
	if u.Token != "" {
		r.Header.Set("authorization", "Bearer "+u.Token)
	}
	resp, err := u.HTTP.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("resolve %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
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

// DefaultStatePath is ~/.config/lard/client-state.json.
func DefaultStatePath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "lard", "client-state.json")
	}
	return "client-state.json"
}
