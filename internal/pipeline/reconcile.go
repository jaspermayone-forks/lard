package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// Verdict is the reconciliation decision for one candidate against its
// neighbors.
type Verdict struct {
	CandidateIndex int    `json:"candidateIndex"`
	Action         string `json:"action"` // NEW | REINFORCE | SUPERSEDE | CONTRADICT
	TargetID       string `json:"targetId,omitempty"`
}

// CandidateWithNeighbors pairs a gated candidate with the active records
// already living at its (scope, key).
type CandidateWithNeighbors struct {
	Index     int
	Candidate GatedCandidate
	Neighbors []*types.Record
}

const reconcileSystem = `You reconcile newly extracted candidate facts against existing memory records.

For each candidate, decide one action relative to the existing records at the same (scope, key):

- NEW: no neighbor covers this dimension. Insert it.
- REINFORCE: a neighbor states the same value. Do not duplicate; just reinforce it.
- SUPERSEDE: a neighbor holds an OLDER value on the same SINGLE-VALUED dimension. People change editors; they do not hold two at once. The new fact replaces the old.
- CONTRADICT: the facts conflict but it is NOT a clean replacement: a multi-valued or context-dependent dimension, or conflicting evidence of similar recency. Keep both and surface the tension for the user.

You may NOT supersede a record with source "user". If a candidate conflicts with a user-pinned record, mark it CONTRADICT (or drop it by omitting it from the output) but never SUPERSEDE.

Input is a JSON array: {"index", "fact", "key", "neighbors": [{"id", "value", "source", "confidence"}]}.
Output a JSON array (no prose): {"candidateIndex": <int>, "action": "NEW"|"REINFORCE"|"SUPERSEDE"|"CONTRADICT", "targetId": "<neighbor id or empty>"}`

// Classify batches all of a session's candidates into one LLM call.
func Classify(ctx context.Context, client *llm.Client, batch []CandidateWithNeighbors) ([]Verdict, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("[")
	for i, c := range batch {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"index":%d,"fact":%q,"key":%q,"neighbors":[`, c.Index, c.Candidate.Candidate.Fact, c.Candidate.Candidate.Key)
		for j, n := range c.Neighbors {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%q,"value":%q,"source":%q,"confidence":%.2f}`, n.ID, n.Value, string(n.Source), n.Confidence)
		}
		b.WriteString("]}")
	}
	b.WriteString("]")
	raw, err := client.Complete(ctx, reconcileSystem, b.String(), 8192)
	if err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}
	var verdicts []Verdict
	if err := llm.ExtractJSON(raw, &verdicts); err != nil {
		// Models sometimes return a bare object when there is one verdict.
		var single Verdict
		if err2 := llm.ExtractJSON(raw, &single); err2 == nil && single.Action != "" {
			return []Verdict{single}, nil
		}
		return nil, fmt.Errorf("classify parse: %w (raw: %.300s)", err, raw)
	}
	return verdicts, nil
}

const (
	reinforceAlpha = 0.3 // confidence rise on repetition
	supersedeBeta  = 0.5 // decay applied to the superseded record
)

// Apply executes a verdict against the store. It is the only place records
// are written during consolidation.
func (a *Applier) Apply(v Verdict, c GatedCandidate, neighbors []*types.Record) error {
	byID := map[string]*types.Record{}
	for _, n := range neighbors {
		byID[n.ID] = n
	}
	switch v.Action {
	case "NEW":
		return a.insert(c)
	case "REINFORCE":
		n := byID[v.TargetID]
		if n == nil {
			return a.insert(c)
		}
		n.Confidence = n.Confidence + (1-n.Confidence)*reinforceAlpha
		n.LastSeenAt = a.now()
		return a.store.UpsertRecord(n)
	case "SUPERSEDE":
		n := byID[v.TargetID]
		if n == nil || n.Source == types.SourceUser {
			// Never overwrite the user; if target is missing, fall back to NEW.
			if n == nil {
				slog.Warn("reconcile: supersede target not found", "key", c.Candidate.Key, "target", v.TargetID)
				return a.insert(c)
			}
			return nil
		}
		rec := a.newRecord(c)
		rec.Supersedes = []string{n.ID}
		if err := a.store.UpsertRecord(rec); err != nil {
			return err
		}
		n.Status = types.StatusSuperseded
		n.Confidence = n.Confidence * supersedeBeta
		return a.store.UpsertRecord(n)
	case "CONTRADICT":
		n := byID[v.TargetID]
		if n == nil {
			return a.insert(c)
		}
		rec := a.newRecord(c)
		rec.Status = types.StatusContradicted
		rec.Contradicts = []string{n.ID}
		rec.Confidence = rec.Confidence * 0.8
		if err := a.store.UpsertRecord(rec); err != nil {
			return err
		}
		n.Status = types.StatusContradicted
		n.Contradicts = appendUnique(n.Contradicts, rec.ID)
		n.Confidence = n.Confidence * 0.8
		return a.store.UpsertRecord(n)
	default:
		return a.insert(c)
	}
}

func (a *Applier) insert(c GatedCandidate) error {
	return a.store.UpsertRecord(a.newRecord(c))
}

// newRecord builds a record for a candidate. Timing inherits the session's
// clock (a.now, wired to the session's endedAt during consolidation), not
// wall-clock — a fact stated in November should look old after a January
// backfill, not fresh.
func (a *Applier) newRecord(c GatedCandidate) *types.Record {
	klass := types.ClassDynamic
	if c.Candidate.Class == "static" {
		klass = types.ClassStatic
	}
	t := a.now()
	return &types.Record{
		Scope:      c.Scope,
		Key:        c.Candidate.Key,
		Value:      c.Candidate.Fact,
		Confidence: c.Candidate.Confidence,
		Class:      klass,
		Source:     types.SourceBatch,
		Status:     types.StatusActive,
		CreatedAt:  t,
		LastSeenAt: t,
	}
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
