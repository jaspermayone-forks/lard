package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

const (
	extractConcurrency   = 3
	synthConcurrency     = 3
	extractBatchSize     = 100
)

// Consolidator runs the extract -> group -> synthesize pipeline.
type Consolidator struct {
	store   *store.Store
	llm     *llm.Client
	Resolve func(hints *types.ProjectHints) (projectID string, err error)
}

// New builds a Consolidator.
func New(st *store.Store, client *llm.Client, resolve func(*types.ProjectHints) (string, error)) *Consolidator {
	return &Consolidator{store: st, llm: client, Resolve: resolve}
}

// Run drains all pending work: extract facts from every unextracted session,
// then synthesize every dirty subject. Both phases are checkpointed, so a
// crash or re-run resumes cleanly. limit>0 caps sessions extracted this run.
func (c *Consolidator) Run(ctx context.Context, limit int) (extracted int, err error) {
	extracted, err = c.extractPhase(ctx, limit)
	if err != nil {
		return extracted, err
	}
	if err := c.synthesizePhase(ctx); err != nil {
		return extracted, err
	}
	return extracted, nil
}

// extractPhase runs extraction across unextracted sessions in parallel,
// persisting facts per session. Each session is an independent checkpoint.
func (c *Consolidator) extractPhase(ctx context.Context, limit int) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		want := extractBatchSize
		if limit > 0 && limit-total < want {
			want = limit - total
		}
		if want <= 0 {
			return total, nil
		}
		sessions, err := c.store.ListUnextractedSessions(want)
		if err != nil {
			return total, err
		}
		if len(sessions) == 0 {
			return total, nil
		}
		// The subject listing is shared read-only context for routing.
		listing, err := c.store.ListSubjects()
		if err != nil {
			return total, err
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, extractConcurrency)
		var mu sync.Mutex
		done := 0
		for _, sess := range sessions {
			wg.Add(1)
			sem <- struct{}{}
			go func(s *store.PendingSession) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := c.extractSession(ctx, s, listing); err != nil {
					slog.Error("extract: session failed", "session", s.SessionID, "error", err)
					// Mark extracted-with-no-facts so a poison session doesn't
					// wedge the queue; it simply contributes nothing.
					_ = c.store.SaveFacts(s.Source, s.SessionID, time.Unix(s.EndedAt, 0).UTC(), nil)
					return
				}
				mu.Lock()
				done++
				mu.Unlock()
			}(sess)
		}
		wg.Wait()
		total += len(sessions)
		slog.Info("extract: batch complete", "sessions", total, "with_facts", done)
	}
}

func (c *Consolidator) extractSession(ctx context.Context, s *store.PendingSession, listing []types.SubjectListing) error {
	projectName := ""
	projectID := ""
	if s.Hints != nil {
		if c.Resolve != nil {
			if pid, err := c.Resolve(s.Hints); err == nil {
				projectID = pid
			}
		}
		projectName = s.Hints.Name
		if projectName == "" && s.Hints.GitRemote != "" {
			projectName = deriveName(s.Hints.GitRemote)
		}
	}
	batch := &types.SessionBatch{
		SessionID:    s.SessionID,
		Source:       s.Source,
		Turns:        s.Turns,
		ProjectHints: &types.ProjectHints{Name: projectName},
	}
	cands, err := Extract(ctx, c.llm, batch, listing)
	if err != nil {
		return err
	}
	// The session's own project slug: only the area matching it gets linked
	// to the registry id. Areas merely mentioned in passing stay unlinked.
	sessionSlug := slugify(projectName)
	sessionDate := time.Unix(s.EndedAt, 0).UTC()
	var facts []types.Fact
	for _, cand := range cands {
		if cand.Sensitivity != "" && sensitivityBlocklist[cand.Sensitivity] {
			continue
		}
		kind := types.SubjectKind(cand.SubjectKind)
		name := cand.SubjectName
		if kind == types.KindProfile {
			name = "profile"
		} else {
			name = slugify(name)
		}
		if name == "" {
			continue
		}
		tag := types.ProvenanceTag(cand.Tag)
		if tag == "" {
			tag = types.TagStated
		}
		facts = append(facts, types.Fact{
			Source:      s.Source,
			SessionID:   s.SessionID,
			SubjectKind: kind,
			SubjectName: name,
			Text:        cand.Text,
			Tag:         tag,
		})
		// Link the area to the registry only when it IS the session's project.
		linkID := ""
		if kind == types.KindArea && sessionSlug != "" && name == sessionSlug {
			linkID = projectID
		}
		c.ensureSubject(kind, name, cand.Description, cand.Aliases, linkID)
	}
	return c.store.SaveFacts(s.Source, s.SessionID, sessionDate, facts)
}

// ensureSubject creates a placeholder subject file if none exists yet, so it
// appears in the listing immediately. Synthesis fills the body later.
func (c *Consolidator) ensureSubject(kind types.SubjectKind, name, desc string, aliases []string, projectID string) {
	existing, err := c.store.ResolveSubject(kind, name)
	if err == nil && existing != "" {
		return
	}
	sub, err := c.store.GetSubject(kind, name)
	if err == nil && sub != nil {
		return
	}
	newSub := &types.Subject{
		Kind:        kind,
		Name:        name,
		Description: desc,
		Aliases:     aliases,
		Body:        "",
	}
	if kind == types.KindArea {
		newSub.ProjectID = projectID
	}
	_ = c.store.PutSubject(newSub, 0)
}

// synthesizePhase rewrites every subject that has facts newer than its last
// synthesis, in parallel across subjects (each writes a distinct file).
func (c *Consolidator) synthesizePhase(ctx context.Context) error {
	dirty, err := c.store.DirtySubjects()
	if err != nil {
		return err
	}
	if len(dirty) == 0 {
		slog.Info("synthesize: nothing dirty")
		return nil
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, synthConcurrency)
	var mu sync.Mutex
	var ok, failed int
	for _, d := range dirty {
		kind, name := types.SubjectKind(d[0]), d[1]
		wg.Add(1)
		sem <- struct{}{}
		go func(kind types.SubjectKind, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := c.synthesizeSubject(ctx, kind, name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				slog.Error("synthesize: subject failed", "kind", kind, "name", name, "error", err)
				return
			}
			ok++
			slog.Info("synthesize: subject done", "kind", kind, "name", name, "progress", ok+failed, "total", len(dirty))
		}(kind, name)
	}
	wg.Wait()
	slog.Info("synthesize: complete", "subjects", len(dirty), "written", ok, "failed", failed)
	return nil
}

func (c *Consolidator) synthesizeSubject(ctx context.Context, kind types.SubjectKind, name string) error {
	facts, maxID, err := c.store.FactsForSubject(kind, name)
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		return nil
	}
	sub, err := c.store.GetSubject(kind, name)
	if err != nil {
		return err
	}
	if sub == nil {
		sub = &types.Subject{Kind: kind, Name: name}
	}
	body, err := Synthesize(ctx, c.llm, sub, facts)
	if err != nil {
		return err
	}
	sub.Body = body
	if sub.Description == "" {
		sub.Description = name
	}
	return c.store.PutSubject(sub, maxID)
}

func deriveName(remote string) string {
	r := NormalizeRemote(remote)
	if i := strings.LastIndex(r, "/"); i >= 0 && i < len(r)-1 {
		return r[i+1:]
	}
	return r
}
