package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/taciturnaxolotl/lard/internal/auth"
	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/pipeline"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/tenant"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// ErrNoIdentity means a request reached a multi-user server without an
// authenticated identity to attribute it to. There is no sensible default
// memory to serve, so the request is refused rather than guessed at.
var ErrNoIdentity = errors.New("no authenticated identity; multi-user lard needs oauth (or a configured primary user)")

// Tenant is one user's memory: their store, their project registry, and their
// consolidation state. Everything that used to hang off the server hangs off a
// tenant instead, which is what makes the single-user and multi-user cases the
// same code.
type Tenant struct {
	key      string
	subject  string
	st       *store.Store
	registry *pipeline.Registry
	llm      *llm.Client
	auto     *autoConsolidator

	// Single-flight consolidation. A manual /consolidate and the quiet
	// timer both funnel through one job, so concurrent callers never start
	// competing passes, and a caller going away doesn't kill the pass.
	consolMu  sync.Mutex
	consolJob *consolidationJob
}

func newTenant(key, subject string, st *store.Store, llmClient *llm.Client) *Tenant {
	return &Tenant{key: key, subject: subject, st: st, registry: pipeline.NewRegistry(st), llm: llmClient}
}

// Key is the tenant's storage slug, or "" for a single-user server.
func (t *Tenant) Key() string { return t.key }

// Subject is the identity that owns this tenant.
func (t *Tenant) Subject() string { return t.subject }

// Store is the tenant's persistence layer.
func (t *Tenant) Store() *store.Store { return t.st }

// Registry resolves project hints within this tenant.
func (t *Tenant) Registry() *pipeline.Registry { return t.registry }

// Consolidator builds a pass over this tenant's store.
func (t *Tenant) Consolidator() *pipeline.Consolidator {
	return pipeline.New(t.st, t.llm, t.registry.Resolve)
}

// ContextBundle assembles the injection bundle: profile in full, the subject
// listing, and (for a project session) that project's area file.
func (t *Tenant) ContextBundle(projectID string) (*types.ContextBundle, error) {
	bundle := &types.ContextBundle{ProjectID: projectID}
	if profile, err := t.st.GetSubject(types.KindProfile, "profile"); err != nil {
		return nil, err
	} else if profile != nil {
		bundle.Profile = profile.Body
	}
	listing, err := t.st.ListSubjects()
	if err != nil {
		return nil, err
	}
	bundle.Listing = listing
	if projectID != "" {
		name, err := t.st.SubjectForProject(projectID)
		if err != nil {
			return nil, err
		}
		if name != "" {
			sub, err := t.st.GetSubject(types.KindArea, name)
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

// enableAuto gives this tenant its own quiet timer. Timers are per tenant
// because ingests are: one user uploading an afternoon of work should not
// reset anyone else's clock or drag their memory through a pass.
func (t *Tenant) enableAuto(after, maxWait time.Duration) {
	if t.llm == nil {
		return
	}
	t.auto = newAutoConsolidator(after, maxWait, func(ctx context.Context) error {
		_, err := t.consolidate(ctx)
		return err
	})
}

func (t *Tenant) stop() {
	if t.auto != nil {
		t.auto.Stop()
	}
}

// MultiUserConfig configures a server that keeps one store per identity.
type MultiUserConfig struct {
	// Layout is where tenant directories live.
	Layout tenant.Layout
	// PrimaryUser owns requests that arrive with no OAuth identity, which is
	// every request under token or none auth. Empty means such requests are
	// refused.
	PrimaryUser string
}

// Server wires tenants to HTTP handlers. In single-user mode one tenant is
// fixed at construction; in multi-user mode tenants are opened on first
// contact and keyed by the caller's identity.
type Server struct {
	llm *llm.Client
	mux *http.ServeMux

	fixed *Tenant // single-user mode

	multi   bool
	layout  tenant.Layout
	primary string

	autoOn      bool
	autoAfter   time.Duration
	autoMaxWait time.Duration

	mu   sync.Mutex
	open map[string]*Tenant
}

// New builds a single-user server over one already-open store. llmClient may
// be nil if consolidation is never triggered from this process.
func New(st *store.Store, llmClient *llm.Client) *Server {
	s := &Server{llm: llmClient, mux: http.NewServeMux()}
	s.fixed = newTenant("", "", st, llmClient)
	s.routes()
	return s
}

// NewMultiUser builds a server that gives every authenticated identity its own
// isolated store, opened on first contact.
func NewMultiUser(cfg MultiUserConfig, llmClient *llm.Client) *Server {
	s := &Server{
		llm:     llmClient,
		mux:     http.NewServeMux(),
		multi:   true,
		layout:  cfg.Layout,
		primary: cfg.PrimaryUser,
		open:    map[string]*Tenant{},
	}
	s.routes()
	return s
}

// TenantFor resolves the caller's tenant, opening its store the first time
// that identity is seen.
func (s *Server) TenantFor(ctx context.Context) (*Tenant, error) {
	if !s.multi {
		return s.fixed, nil
	}
	subject := s.primary
	if id, ok := auth.IdentityFrom(ctx); ok && id.Subject != "" {
		subject = id.Subject
	}
	if subject == "" {
		return nil, ErrNoIdentity
	}
	key := tenant.Slug(subject)

	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.open[key]; ok {
		return t, nil
	}
	// Tenants stay open for the life of the process. A homelab serves a
	// handful of people, one SQLite connection each, and an eviction policy
	// that can close a store out from under a running consolidation pass buys
	// nothing worth that risk.
	fresh := !s.layout.Exists(key)
	st, err := s.layout.Open(key)
	if err != nil {
		return nil, err
	}
	t := newTenant(key, subject, st, s.llm)
	if s.autoOn {
		t.enableAuto(s.autoAfter, s.autoMaxWait)
	}
	s.open[key] = t
	if fresh {
		slog.Info("tenant created", "subject", subject, "dir", s.layout.Dir(key))
	} else {
		slog.Info("tenant opened", "subject", subject, "dir", s.layout.Dir(key))
	}
	return t, nil
}

// tenantOr resolves the caller's tenant, writing the error response itself
// when it cannot. A nil return means the response is already written.
func (s *Server) tenantOr(w http.ResponseWriter, r *http.Request) *Tenant {
	t, err := s.TenantFor(r.Context())
	if err != nil {
		if errors.Is(err, ErrNoIdentity) {
			writeErr(w, http.StatusUnauthorized, err)
		} else {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("open tenant: %w", err))
		}
		return nil
	}
	return t
}

// EnableAutoConsolidate makes tenants consolidate on their own once uploads go
// quiet, so memory stays current without anyone calling /consolidate. No-op
// without an LLM client, since a pass would fail anyway.
func (s *Server) EnableAutoConsolidate(after, maxWait time.Duration) {
	if s.llm == nil {
		slog.Warn("auto-consolidate disabled: no LLM client")
		return
	}
	s.autoOn, s.autoAfter, s.autoMaxWait = true, after, maxWait
	if s.fixed != nil {
		s.fixed.enableAuto(after, maxWait)
	}
	slog.Info("auto-consolidate enabled", "quiet_period", after, "max_wait", maxWait)
}

// StopAutoConsolidate cancels every pending scheduled pass.
func (s *Server) StopAutoConsolidate() {
	s.eachTenant((*Tenant).stop)
}

// Close stops timers and closes every open store.
func (s *Server) Close() error {
	var err error
	s.eachTenant(func(t *Tenant) {
		t.stop()
		if cerr := t.st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	})
	return err
}

func (s *Server) eachTenant(fn func(*Tenant)) {
	if s.fixed != nil {
		fn(s.fixed)
	}
	s.mu.Lock()
	open := make([]*Tenant, 0, len(s.open))
	for _, t := range s.open {
		open = append(open, t)
	}
	s.mu.Unlock()
	for _, t := range open {
		fn(t)
	}
}
