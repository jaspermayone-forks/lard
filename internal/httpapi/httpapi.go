// Package httpapi is lard's full HTTP surface: context bundle, KV memory,
// ingest, consolidation triggers, and the project registry. The MCP server
// is a thin wrapper over the same store paths.
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
	llm      *llm.Client // nil disables /consolidate LLM work
	mux      *http.ServeMux
}

// New builds the HTTP server. llmClient may be nil if consolidation is
// never triggered from this process.
func New(st *store.Store, llmClient *llm.Client) *Server {
	s := &Server{st: st, registry: pipeline.NewRegistry(st), llm: llmClient, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

// Registry exposes the project registry (used by the MCP server).
func (s *Server) Registry() *pipeline.Registry { return s.registry }

// Store exposes the store (used by the MCP server).
func (s *Server) Store() *store.Store { return s.st }

// Consolidator builds a consolidator over the same store and registry.
func (s *Server) Consolidator() *pipeline.Consolidator {
	return pipeline.New(s.st, s.llm, s.registry.Resolve)
}

// RenderScope re-renders all docs for a scope from its active records.
func (s *Server) RenderScope(scope types.Scope) error {
	prefix := "profile"
	if scope.Kind == types.ScopeProject {
		prefix = "project/" + scope.ProjectID
	}
	return pipeline.New(s.st, nil, nil).RenderScope(prefix)
}

// RenderSessionLog appends an agent-authored note to a session-log doc
// namespace (project/<id>/session-log/<date>) and re-renders it.
func (s *Server) RenderSessionLog(namespace, note string) error {
	existing, err := s.st.GetDoc(namespace)
	if err != nil {
		return err
	}
	var b strings.Builder
	if existing == "" {
		date := namespace[strings.LastIndex(namespace, "/")+1:]
		fmt.Fprintf(&b, "# Session log %s\n\n", date)
	} else {
		b.WriteString(strings.TrimRight(existing, "\n"))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "- %s\n", note)
	return s.st.PutDoc(namespace, b.String())
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	// Context bundle (§6.2).
	s.mux.HandleFunc("GET /context", s.handleContext)

	// KV memory (§6.3).
	s.mux.HandleFunc("GET /memory/{namespace...}", s.handleMemoryGet)
	s.mux.HandleFunc("PUT /memory/{namespace...}", s.handleMemoryPut)
	s.mux.HandleFunc("DELETE /memory/{namespace...}", s.handleMemoryDelete)
	s.mux.HandleFunc("POST /memory/{namespace...}", s.handleMemoryPost)

	// Ingest & consolidate (§6.4).
	s.mux.HandleFunc("POST /ingest", s.handleIngest)
	s.mux.HandleFunc("POST /consolidate", s.handleConsolidate)

	// Project registry (§4.1).
	s.mux.HandleFunc("POST /projects/resolve", s.handleProjectResolve)
	s.mux.HandleFunc("GET /projects", s.handleProjectList)
	s.mux.HandleFunc("POST /projects/{id}/aliases", s.handleProjectAlias)
	s.mux.HandleFunc("POST /projects/merge", s.handleProjectMerge)

	// Unresolved contradictions (§7, §10).
	s.mux.HandleFunc("GET /conflicts", s.handleConflicts)
}

// --- context ---

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project")
	// Also accept hints so a client without a canonical id can resolve first.
	if projectID == "" {
		hints := hintsFromQuery(r)
		if hints != nil {
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

// ContextBundle assembles the injection bundle for a project ("" = profile only).
func (s *Server) ContextBundle(projectID string) (*types.ContextBundle, error) {
	bundle := &types.ContextBundle{ProjectID: projectID}
	profile, err := s.renderOrStored("profile")
	if err != nil {
		return nil, err
	}
	bundle.Profile = profile
	if projectID != "" {
		project, err := s.renderOrStored("project/" + projectID)
		if err != nil {
			return nil, err
		}
		bundle.Project = project
		log, err := s.sessionLog(projectID)
		if err != nil {
			return nil, err
		}
		bundle.SessionLog = log
	}
	return bundle, nil
}

// renderOrStored concatenates the stored docs under a scope prefix.
func (s *Server) renderOrStored(prefix string) (string, error) {
	nss, err := s.st.ListDocNamespaces(prefix)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ns := range nss {
		// session-log docs ship separately.
		if strings.Contains(ns, "/session-log/") {
			continue
		}
		body, err := s.st.GetDoc(ns)
		if err != nil {
			return "", err
		}
		if !hasContent(body) {
			continue
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// hasContent reports whether a rendered doc carries any records (i.e. is
// more than the empty placeholder).
func hasContent(doc string) bool {
	return strings.Contains(doc, "\n- ")
}

// sessionLog returns the most recent session-log docs for a project.
func (s *Server) sessionLog(projectID string) (string, error) {
	nss, err := s.st.ListDocNamespaces("project/" + projectID + "/session-log")
	if err != nil {
		return "", err
	}
	if len(nss) == 0 {
		return "", nil
	}
	// Namespaces are date-stamped; take the latest few.
	const keep = 3
	start := 0
	if len(nss) > keep {
		start = len(nss) - keep
	}
	var b strings.Builder
	for _, ns := range nss[start:] {
		body, err := s.st.GetDoc(ns)
		if err != nil {
			return "", err
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// --- memory KV ---

// splitNamespace parses a namespace path of the form
// "profile" | "profile/preferences" | "project/<id>" | "project/<id>/conventions"
// plus an optional trailing record key.
func splitNamespace(path string) (namespace string, recordKey string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "project" {
		// project/<id>/<doc...> — the doc is parts[2], deeper parts are the key.
		if len(parts) == 3 {
			return strings.Join(parts, "/"), ""
		}
		return strings.Join(parts[:3], "/"), strings.Join(parts[3:], "/")
	}
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/"), strings.Join(parts[2:], "/")
	}
	return path, ""
}

func (s *Server) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	if strings.HasSuffix(ns, "/records") {
		ns = strings.TrimSuffix(ns, "/records")
		s.writeRecords(w, r, ns)
		return
	}
	body, err := s.st.GetDoc(ns)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if body == "" {
		// Try rendering on the fly from records for namespaces never consolidated.
		if r.URL.Query().Get("render") == "1" {
			s.writeRecords(w, r, ns)
			return
		}
		writeErr(w, 404, errors.New("namespace not found"))
		return
	}
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Write([]byte(body))
}

func (s *Server) writeRecords(w http.ResponseWriter, r *http.Request, ns string) {
	scopeKind, projectID, key := parseScope(ns)
	recs, err := s.st.ListRecords(scopeKind, projectID, key, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, recs)
}

// parseScope maps a namespace to (scopeKind, projectID, key).
func parseScope(ns string) (kind, projectID, key string) {
	ns, key = splitNamespace(ns)
	if ns == "profile" || strings.HasPrefix(ns, "profile/") {
		return string(types.ScopeProfile), "", key
	}
	if strings.HasPrefix(ns, "project/") {
		parts := strings.Split(ns, "/")
		if len(parts) >= 2 {
			return string(types.ScopeProject), parts[1], key
		}
	}
	return "", "", key
}

func (s *Server) handleMemoryPut(w http.ResponseWriter, r *http.Request) {
	ns, recordKey := splitNamespace(r.PathValue("namespace"))
	scopeKind, projectID, _ := parseScope(ns)
	if scopeKind == "" {
		writeErr(w, 400, errors.New("namespace must be profile/... or project/<id>/..."))
		return
	}
	var body struct {
		Value      string  `json:"value"`
		Confidence float64 `json:"confidence"`
		Class      string  `json:"klass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Value == "" {
		writeErr(w, 400, errors.New("value required"))
		return
	}
	if recordKey == "" {
		recordKey = r.URL.Query().Get("key")
	}
	if recordKey == "" {
		writeErr(w, 400, errors.New("record key required: PUT /memory/{namespace}/{key}"))
		return
	}
	conf := body.Confidence
	if conf <= 0 || conf > 1 {
		conf = 1.0 // user assertions are authoritative
	}
	rec := &types.Record{
		Scope:      types.Scope{Kind: types.ScopeKind(scopeKind), ProjectID: projectID},
		Key:        recordKey,
		Value:      body.Value,
		Confidence: conf,
		Class:      types.ClassStatic,
		Source:     types.SourceUser,
		Status:     types.StatusActive,
	}
	if body.Class == "dynamic" {
		rec.Class = types.ClassDynamic
	}
	if err := s.st.UpsertRecord(rec); err != nil {
		writeErr(w, 500, err)
		return
	}
	// Re-render the namespace doc so the KV view reflects the edit.
	_ = pipeline.New(s.st, nil, nil).RenderScope(nsScopePrefix(scopeKind, projectID))
	writeJSON(w, 200, rec)
}

func (s *Server) handleMemoryPost(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	scopeKind, projectID, _ := parseScope(ns)
	if scopeKind == "" {
		writeErr(w, 400, errors.New("namespace must be profile/... or project/<id>/..."))
		return
	}
	var body struct {
		Observations []struct {
			Key        string  `json:"key"`
			Value      string  `json:"value"`
			Confidence float64 `json:"confidence"`
			Class      string  `json:"klass"`
		} `json:"observations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	var out []*types.Record
	for _, o := range body.Observations {
		if o.Key == "" || o.Value == "" {
			continue
		}
		conf := o.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.8
		}
		rec := &types.Record{
			Scope:      types.Scope{Kind: types.ScopeKind(scopeKind), ProjectID: projectID},
			Key:        o.Key,
			Value:      o.Value,
			Confidence: conf,
			Class:      types.ClassDynamic,
			Source:     types.SourceAgent,
			Status:     types.StatusActive,
		}
		if o.Class == "static" {
			rec.Class = types.ClassStatic
		}
		if err := s.st.UpsertRecord(rec); err != nil {
			writeErr(w, 500, err)
			return
		}
		out = append(out, rec)
	}
	_ = pipeline.New(s.st, nil, nil).RenderScope(nsScopePrefix(scopeKind, projectID))
	writeJSON(w, 200, out)
}

func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	ns, recordKey := splitNamespace(r.PathValue("namespace"))
	scopeKind, projectID, _ := parseScope(ns)
	if scopeKind == "" || recordKey == "" {
		writeErr(w, 400, errors.New("DELETE /memory/{namespace}/{key}"))
		return
	}
	n, err := s.st.SoftDeleteKey(types.Scope{Kind: types.ScopeKind(scopeKind), ProjectID: projectID}, recordKey)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	_ = pipeline.New(s.st, nil, nil).RenderScope(nsScopePrefix(scopeKind, projectID))
	writeJSON(w, 200, map[string]int64{"deleted": n})
}

func nsScopePrefix(kind, projectID string) string {
	if kind == string(types.ScopeProject) {
		return "project/" + projectID
	}
	return "profile"
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
	// Resolve project hints eagerly so /context works before consolidation.
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
		// Detach from the request context: the response returns immediately
		// and the pass outlives it.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		c := s.Consolidator()
		n, err := c.Run(ctx, 0) // 0 = drain all pending sessions
		if err != nil {
			slog.Error("consolidate", "error", err)
		} else {
			slog.Info("consolidate done", "processed", n)
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
	created := false
	if id != "" {
		if p, _ := s.st.GetProject(id); p != nil && time.Since(mustParseTime(p.CreatedAt)) < 5*time.Second {
			created = true
		}
	}
	writeJSON(w, 200, map[string]any{"projectId": id, "created": created})
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

func (s *Server) handleProjectMerge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Into string `json:"into"`
		From string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Into == "" || body.From == "" || body.Into == body.From {
		writeErr(w, 400, errors.New("into and from must differ"))
		return
	}
	if err := s.st.MergeProjects(body.Into, body.From); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	pairs, err := s.st.ListContradictions()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, pairs)
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

func mustParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
