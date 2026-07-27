package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// Applier executes reconciliation verdicts against the store.
type Applier struct {
	store *store.Store
	// at is the clock records are stamped with: the session's endedAt during
	// consolidation, wall-clock for live writes.
	at time.Time
}

func (a *Applier) now() time.Time {
	if !a.at.IsZero() {
		return a.at
	}
	return time.Now().UTC()
}

// Consolidator runs the nightly pass over pending sessions.
type Consolidator struct {
	store *store.Store
	llm   *llm.Client
	// Resolve maps project hints to a canonical project id. Wired to the
	// registry; injected so consolidation never resolves identity itself.
	Resolve func(hints *types.ProjectHints) (projectID string, err error)
}

// New builds a Consolidator.
func New(st *store.Store, client *llm.Client, resolve func(*types.ProjectHints) (string, error)) *Consolidator {
	return &Consolidator{store: st, llm: client, Resolve: resolve}
}

const defaultConcurrency = 3

// batchSize caps how many sessions one batch fans out at once.
const batchSize = 100

// Run drains all pending sessions in batches until none remain (or limit,
// if positive, is reached). Each batch runs concurrently; documents render
// and decay applies once per batch. Sessions that fail (e.g. malformed LLM
// output) are not retried within a single Run to avoid a hot loop; they
// stay pending for the next scheduled pass.
func (c *Consolidator) Run(ctx context.Context, limit int) (processed int, err error) {
	seen := map[string]bool{} // session ids already attempted this Run
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		remaining := batchSize
		if limit > 0 {
			if processed >= limit {
				return processed, nil
			}
			if limit-processed < remaining {
				remaining = limit - processed
			}
		}
		n, attempted, err := c.runBatch(ctx, remaining, seen)
		if err != nil {
			return processed, err
		}
		processed += n
		// Stop when a batch attempts no new sessions: either the queue is
		// empty or everything left is a session we already tried and failed.
		if attempted == 0 {
			return processed, nil
		}
		slog.Info("consolidate: batch complete", "processed", processed)
	}
}

// runBatch processes up to limit pending sessions concurrently: extract,
// gate, reconcile, then regenerates touched documents and decays stale
// records. Sessions in `seen` are skipped (already tried this Run).
// Returns (succeeded, attempted).
func (c *Consolidator) runBatch(ctx context.Context, limit int, seen map[string]bool) (processed, attempted int, err error) {
	if limit <= 0 {
		limit = batchSize
	}
	// Over-fetch so skipping already-seen sessions still fills a batch.
	all, err := c.store.ListPendingSessions(limit + len(seen))
	if err != nil {
		return 0, 0, fmt.Errorf("list pending: %w", err)
	}
	var pending []*store.PendingSession
	for _, s := range all {
		if seen[s.SessionID] {
			continue
		}
		pending = append(pending, s)
		if len(pending) >= limit {
			break
		}
	}
	if len(pending) == 0 {
		slog.Info("consolidate: no pending sessions")
		return 0, 0, nil
	}
	for _, s := range pending {
		seen[s.SessionID] = true
	}
	attempted = len(pending)

	type result struct {
		sess   *store.PendingSession
		err    error
		scopes map[string]bool
	}

	sem := make(chan struct{}, defaultConcurrency)
	resCh := make(chan result, len(pending))

	// Fan out sessions across workers with a small stagger to avoid
	// thundering-herd rate limits on the LLM provider.
	for i, sess := range pending {
		if err := ctx.Err(); err != nil {
			return processed, attempted, err
		}
		// Stagger submissions: first batch goes immediately, then 200ms apart.
		if i >= defaultConcurrency {
			time.Sleep(200 * time.Millisecond)
		}
		sem <- struct{}{} // acquire slot
		go func(s *store.PendingSession) {
			defer func() { <-sem }() // release slot

			applier := &Applier{store: c.store, at: time.Unix(s.EndedAt, 0).UTC()}
			projectID := ""
			if s.Hints != nil && c.Resolve != nil {
				pid, err := c.Resolve(s.Hints)
				if err != nil {
					slog.Warn("consolidate: resolve project", "session", s.SessionID, "error", err)
				} else {
					projectID = pid
				}
			}
			touched := map[string]bool{}
			err := c.processSession(ctx, applier, s, projectID, touched)
			resCh <- result{s, err, touched}
		}(sess)
	}

	// Collect results.
	touchedScopes := map[string]bool{}
	for range pending {
		r := <-resCh
		if r.err != nil {
			slog.Error("consolidate: session failed", "source", r.sess.Source, "session", r.sess.SessionID, "error", r.err)
			continue
		}
		if err := c.store.MarkConsolidated(r.sess.Source, r.sess.SessionID); err != nil {
			return processed, attempted, fmt.Errorf("mark consolidated: %w", err)
		}
		processed++
		for k := range r.scopes {
			touchedScopes[k] = true
		}
	}

	// Render all touched scopes once at the end (incremental rendering per-session
	// would race with parallel workers).
	for scopeKey := range touchedScopes {
		if err := c.RenderScope(scopeKey); err != nil {
			slog.Error("consolidate: render", "scope", scopeKey, "error", err)
		}
	}

	if err := c.Decay(); err != nil {
		slog.Error("consolidate: decay", "error", err)
	}
	return processed, attempted, nil
}

func (c *Consolidator) processSession(ctx context.Context, applier *Applier, sess *store.PendingSession, projectID string, touched map[string]bool) error {
	batch := &types.SessionBatch{
		SessionID: sess.SessionID,
		Source:    sess.Source,
		Turns:     sess.Turns,
	}
	cands, err := Extract(ctx, c.llm, batch)
	if err != nil {
		return err
	}
	// Fetch the current profile doc for scope-routing context.
	profileCtx, _ := c.store.GetDoc("profile/preferences")
	if profileCtx == "" {
		profileCtx, _ = c.store.GetDoc("profile/identity")
	}
	gated := Gate(cands, projectID, profileCtx)
	if len(gated) == 0 {
		return nil
	}

	// Attach neighbors (active records at the same scope+key).
	var withNeighbors []CandidateWithNeighbors
	for i, g := range gated {
		neighbors, err := c.store.ListRecords(string(g.Scope.Kind), g.Scope.ProjectID, g.Candidate.Key, string(types.StatusActive))
		if err != nil {
			return err
		}
		withNeighbors = append(withNeighbors, CandidateWithNeighbors{Index: i, Candidate: g, Neighbors: neighbors})
		touched[g.Scope.String()] = true
	}

	verdicts, err := Classify(ctx, c.llm, withNeighbors)
	if err != nil {
		return err
	}
	byIndex := map[int]Verdict{}
	for _, v := range verdicts {
		byIndex[v.CandidateIndex] = v
	}
	for _, wn := range withNeighbors {
		v, ok := byIndex[wn.Index]
		if !ok {
			v = Verdict{CandidateIndex: wn.Index, Action: "NEW"}
		}
		if err := applier.Apply(v, wn.Candidate, wn.Neighbors); err != nil {
			slog.Error("consolidate: apply", "key", wn.Candidate.Candidate.Key, "error", err)
		}
	}
	return nil
}
