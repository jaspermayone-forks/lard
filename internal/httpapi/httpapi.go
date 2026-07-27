// Package httpapi is lard's HTTP surface: the context bundle, the subject
// memory file operations (list/read/write/append/edit/delete), ingest,
// consolidation, and the project registry. The MCP server wraps the same
// store paths.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/pipeline"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// Server wires the store, registry, and consolidator to HTTP handlers.
type Server struct {
	st       *store.Store
	registry *pipeline.Registry
	llm      *llm.Client
	mux      *http.ServeMux
}

// New builds the HTTP server. llmClient may be nil if consolidation is never
// triggered from this process.
func New(st *store.Store, llmClient *llm.Client) *Server {
	s := &Server{st: st, registry: pipeline.NewRegistry(st), llm: llmClient, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler          { return s.mux }
func (s *Server) Registry() *pipeline.Registry    { return s.registry }
func (s *Server) Store() *store.Store             { return s.st }
func (s *Server) Consolidator() *pipeline.Consolidator {
	return pipeline.New(s.st, s.llm, s.registry.Resolve)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	// Context bundle (session start).
	s.mux.HandleFunc("GET /context", s.handleContext)

	// Subject memory file operations.
	s.mux.HandleFunc("GET /memory", s.handleList)
	s.mux.HandleFunc("GET /memory/{path...}", s.handleRead)
	s.mux.HandleFunc("PUT /memory/{path...}", s.handleWrite)
	s.mux.HandleFunc("POST /memory/{path...}", s.handleAppend)
	s.mux.HandleFunc("DELETE /memory/{path...}", s.handleDelete)

	// Ingest & consolidate.
	s.mux.HandleFunc("POST /ingest", s.handleIngest)
	s.mux.HandleFunc("POST /consolidate", s.handleConsolidate)

	// Project registry.
	s.mux.HandleFunc("POST /projects/resolve", s.handleProjectResolve)
	s.mux.HandleFunc("GET /projects", s.handleProjectList)
	s.mux.HandleFunc("POST /projects/{id}/aliases", s.handleProjectAlias)
}

// --- context ---

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		if hints := hintsFromQuery(r); hints != nil {
			pid, err := s.registry.Resolve(hints)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			projectID = pid
		}
	}
	bundle, err := s.ContextBundle(projectID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, bundle)
}

// ContextBundle assembles the injection bundle: profile in full, the subject
// listing, and (for a project session) that project's area file.
func (s *Server) ContextBundle(projectID string) (*types.ContextBundle, error) {
	bundle := &types.ContextBundle{ProjectID: projectID}
	if profile, err := s.st.GetSubject(types.KindProfile, "profile"); err != nil {
		return nil, err
	} else if profile != nil {
		bundle.Profile = profile.Body
	}
	listing, err := s.st.ListSubjects()
	if err != nil {
		return nil, err
	}
	bundle.Listing = listing
	if projectID != "" {
		// Find the area whose project_id matches; fall back to listing scan.
		for _, l := range listing {
			if l.Kind != types.KindArea {
				continue
			}
			sub, err := s.st.GetSubject(types.KindArea, l.Name)
			if err != nil {
				return nil, err
			}
			if sub != nil && sub.ProjectID == projectID {
				bundle.Area = sub.Body
				break
			}
		}
	}
	return bundle, nil
}

// --- subject memory files ---

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	listing, err := s.st.ListSubjects()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, listing)
}

// parsePath maps a request path ("profile", "areas/crush", "profile.md") to
// a (kind, name).
func parsePath(p string) (types.SubjectKind, string, error) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".md")
	if p == "profile" || p == "" {
		return types.KindProfile, "profile", nil
	}
	dir, name, ok := strings.Cut(p, "/")
	if !ok {
		return "", "", fmt.Errorf("path must be profile, areas/<name>, topics/<name>, or people/<name>")
	}
	switch dir {
	case "areas":
		return types.KindArea, name, nil
	case "topics":
		return types.KindTopic, name, nil
	case "people":
		return types.KindPeople, name, nil
	default:
		return "", "", fmt.Errorf("unknown memory folder %q", dir)
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	kind, name, err := parsePath(r.PathValue("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	sub, err := s.st.GetSubject(kind, name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if sub == nil {
		writeErr(w, 404, errors.New("subject not found"))
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, 200, sub)
		return
	}
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Header().Set("x-lard-version", sub.Version)
	w.Write([]byte(sub.Body))
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	kind, name, err := parsePath(r.PathValue("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var body struct {
		Body        string   `json:"body"`
		Description string   `json:"description"`
		Aliases     []string `json:"aliases"`
		Version     string   `json:"version"` // optimistic concurrency
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	existing, err := s.st.GetSubject(kind, name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := checkVersion(existing, body.Version); err != nil {
		writeConflict(w, existing)
		return
	}
	sub := existing
	if sub == nil {
		sub = &types.Subject{Kind: kind, Name: name}
	}
	sub.Body = body.Body
	if body.Description != "" {
		sub.Description = body.Description
	}
	if body.Aliases != nil {
		sub.Aliases = body.Aliases
	}
	if err := s.st.PutSubject(sub, 0); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, sub)
}

func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	kind, name, err := parsePath(r.PathValue("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var body struct {
		Line string `json:"line"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(body.Line) == "" {
		writeErr(w, 400, errors.New("line required"))
		return
	}
	sub, err := s.st.GetSubject(kind, name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if sub == nil {
		sub = &types.Subject{Kind: kind, Name: name, Description: name}
	}
	line := strings.TrimSpace(body.Line)
	if !strings.HasPrefix(line, "-") {
		line = "- " + line
	}
	if sub.Body == "" {
		sub.Body = line
	} else {
		sub.Body = strings.TrimRight(sub.Body, "\n") + "\n" + line
	}
	if err := s.st.PutSubject(sub, 0); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, sub)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	kind, name, err := parsePath(r.PathValue("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.st.DeleteSubject(kind, name); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func checkVersion(existing *types.Subject, want string) error {
	if want == "" || want == "new" {
		return nil // caller opted out of the check
	}
	if existing == nil {
		return nil
	}
	if existing.Version != want {
		return errors.New("version mismatch")
	}
	return nil
}

func writeConflict(w http.ResponseWriter, current *types.Subject) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "version conflict",
		"current": current,
	})
}

// --- ingest & consolidate ---

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req types.IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	n, err := s.st.IngestSessions(req.Collector, req.Sessions)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	for _, sess := range req.Sessions {
		if sess.ProjectHints != nil {
			if _, err := s.registry.Resolve(sess.ProjectHints); err != nil {
				slog.Warn("ingest: resolve project", "session", sess.SessionID, "error", err)
			}
		}
	}
	writeJSON(w, 200, map[string]int{"ingested": n})
}

func (s *Server) handleConsolidate(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeErr(w, 503, errors.New("consolidation unavailable: no LLM client configured"))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		n, err := s.Consolidator().Run(ctx, 0)
		if err != nil {
			slog.Error("consolidate", "error", err)
		} else {
			slog.Info("consolidate done", "extracted", n)
		}
	}()
	writeJSON(w, 202, map[string]string{"status": "started"})
}

// --- projects ---

func (s *Server) handleProjectResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hints *types.ProjectHints `json:"hints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	id, err := s.registry.Resolve(body.Hints)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"projectId": id})
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.st.ListProjects()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, projects)
}

func (s *Server) handleProjectAlias(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Kind == "remote" {
		body.Value = pipeline.NormalizeRemote(body.Value)
	}
	if err := s.st.AddAlias(id, body.Kind, body.Value); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// --- helpers ---

func hintsFromQuery(r *http.Request) *types.ProjectHints {
	q := r.URL.Query()
	h := &types.ProjectHints{
		GitRemote: q.Get("gitRemote"),
		Path:      q.Get("path"),
		Name:      q.Get("name"),
	}
	if h.GitRemote == "" && h.Path == "" && h.Name == "" {
		return nil
	}
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
