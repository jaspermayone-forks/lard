// Package httpapi is lard's HTTP surface: the context bundle, the subject
// memory file operations (list/read/write/append/edit/delete), ingest,
// consolidation, and the project registry. The MCP server wraps the same
// store paths.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/taciturnaxolotl/lard/internal/auth"
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
	auto     *autoConsolidator
}

// New builds the HTTP server. llmClient may be nil if consolidation is never
// triggered from this process.
func New(st *store.Store, llmClient *llm.Client) *Server {
	s := &Server{st: st, registry: pipeline.NewRegistry(st), llm: llmClient, mux: http.NewServeMux()}
	s.routes()
	return s
}

// EnableAutoConsolidate makes the server consolidate on its own once uploads
// go quiet, so memory stays current without anyone calling /consolidate. No-op
// without an LLM client, since a pass would fail anyway.
func (s *Server) EnableAutoConsolidate(after, maxWait time.Duration) {
	if s.llm == nil {
		slog.Warn("auto-consolidate disabled: no LLM client")
		return
	}
	s.auto = newAutoConsolidator(after, maxWait, func(ctx context.Context) error {
		_, err := s.Consolidator().Run(ctx, 0)
		return err
	})
	slog.Info("auto-consolidate enabled", "quiet_period", after, "max_wait", maxWait)
}

// StopAutoConsolidate cancels any pending scheduled pass.
func (s *Server) StopAutoConsolidate() {
	if s.auto != nil {
		s.auto.Stop()
	}
}

func (s *Server) Handler() http.Handler        { return s.mux }
func (s *Server) Registry() *pipeline.Registry { return s.registry }
func (s *Server) Store() *store.Store          { return s.st }
func (s *Server) Consolidator() *pipeline.Consolidator {
	return pipeline.New(s.st, s.llm, s.registry.Resolve)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	// Authenticated echo, so a client can prove its credentials work before
	// installing itself as a background service. /healthz bypasses auth and
	// therefore cannot answer that question.
	s.mux.HandleFunc("GET /whoami", s.handleWhoami)

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
		name, err := s.st.SubjectForProject(projectID)
		if err != nil {
			return nil, err
		}
		if name != "" {
			sub, err := s.st.GetSubject(types.KindArea, name)
			if err != nil {
				return nil, err
			}
			if sub != nil {
				bundle.Area = sub.Body
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

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	kind, name, err := types.ParseSubjectPath(r.PathValue("path"))
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
	kind, name, err := types.ParseSubjectPath(r.PathValue("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var body struct {
		Body        string           `json:"body"`
		Description string           `json:"description"`
		Aliases     types.StringList `json:"aliases"`
		Repos       types.StringList `json:"repos"`   // one remote or many; areas also get registry-linked
		Version     string           `json:"version"` // optimistic concurrency
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	sub, err := pipeline.ApplyPatch(s.st, s.registry, kind, name, pipeline.SubjectPatch{
		Body:        &body.Body,
		Description: body.Description,
		Aliases:     body.Aliases,
		Repos:       body.Repos,
		Version:     body.Version,
	})
	if errors.Is(err, pipeline.ErrVersionConflict) {
		writeConflict(w, sub)
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, sub)
}

func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	kind, name, err := types.ParseSubjectPath(r.PathValue("path"))
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
	sub, err := pipeline.AppendLine(s.st, kind, name, body.Line)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, sub)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	kind, name, err := types.ParseSubjectPath(r.PathValue("path"))
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

func writeConflict(w http.ResponseWriter, current *types.Subject) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "version conflict",
		"current": current,
	})
}

// --- ingest & consolidate ---

// handleWhoami reports the authenticated caller. Reaching this handler at all
// means the credentials are good; the body says which identity they mapped to.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"authenticated": true}
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		out["subject"] = id.Subject
		out["clientId"] = id.ClientID
		out["scopes"] = id.Scopes
	}
	writeJSON(w, 200, out)
}

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
	// New work landed: start (or extend) the quiet period before consolidating.
	if n > 0 && s.auto != nil {
		s.auto.Trigger()
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
