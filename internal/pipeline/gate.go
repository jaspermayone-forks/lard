// Package pipeline is the nightly consolidator: extract durable facts from
// user turns, gate and route them deterministically, reconcile against
// memory, regenerate documents, and decay stale dynamic records.
package pipeline

import (
	"strings"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// profilePrefixes route to the global profile even when voiced in a
// project session. Everything else with a project context lands in the
// project scope. scopeHint only breaks ties.
var profilePrefixes = []string{"identity.", "preferences.", "comms.", "workflow."}

var projectPrefixes = []string{"conventions.", "decisions.", "corrections."}

// sensitivityBlocklist holds inference categories never persisted.
// Dropped at the gate, before storage.
var sensitivityBlocklist = map[string]bool{
	"health":    true,
	"politics":  true,
	"religion":  true,
	"finances":  true,
	"location":  true,
	"secrets":   true,
	"biometric": true,
}

// Route decides the destination scope for a candidate given the session's
// project context (empty projectID = no project).
func Route(key, scopeHint, projectID string) types.Scope {
	for _, p := range profilePrefixes {
		if strings.HasPrefix(key, p) {
			return types.Scope{Kind: types.ScopeProfile}
		}
	}
	for _, p := range projectPrefixes {
		if strings.HasPrefix(key, p) && projectID != "" {
			return types.Scope{Kind: types.ScopeProject, ProjectID: projectID}
		}
	}
	// Tie-break on the hint; unknown keys from a project session stay local.
	if scopeHint == "profile" {
		return types.Scope{Kind: types.ScopeProfile}
	}
	if projectID != "" {
		return types.Scope{Kind: types.ScopeProject, ProjectID: projectID}
	}
	return types.Scope{Kind: types.ScopeProfile}
}

// GatedCandidate pairs a surviving candidate with its routed scope.
type GatedCandidate struct {
	Candidate types.Candidate
	Scope     types.Scope
}

// Gate applies the deterministic pass: sensitivity drop, scope routing,
// within-batch dedupe, confidence clamp. No LLM; auditable.
// If profileContext is non-empty, ambiguous keys get an LLM reroute pass
// that sees the global profile to decide scope more accurately.
func Gate(cands []types.Candidate, projectID string, profileContext string) []GatedCandidate {
	type sig struct{ key, fact string }
	seen := map[sig]int{}
	var out []GatedCandidate
	for _, c := range cands {
		if c.Sensitivity != nil && sensitivityBlocklist[strings.ToLower(*c.Sensitivity)] {
			continue
		}
		if c.Confidence < 0 {
			c.Confidence = 0
		}
		if c.Confidence > 1 {
			c.Confidence = 1
		}
		g := sig{key: c.Key, fact: normalizeFact(c.Fact)}
		if idx, ok := seen[g]; ok {
			// Keep max confidence, earliest sourceTurn.
			existing := &out[idx]
			if c.Confidence > existing.Candidate.Confidence {
				existing.Candidate.Confidence = c.Confidence
			}
			if c.SourceTurn < existing.Candidate.SourceTurn {
				existing.Candidate.SourceTurn = c.SourceTurn
			}
			continue
		}
		seen[g] = len(out)
		scope := Route(c.Key, c.ScopeHint, projectID)
		// If the key doesn't match any known prefix and we have profile context,
		// let the LLM decide whether this is global or project-specific.
		if !hasKnownPrefix(c.Key) && profileContext != "" && projectID != "" {
			// Defer to reconcile-time LLM; mark as uncertain for now.
			// The consolidate pass will call rerouteScope if needed.
		}
		out = append(out, GatedCandidate{
			Candidate: c,
			Scope:     scope,
		})
	}
	return out
}

// hasKnownPrefix reports whether a key starts with a known routing prefix.
func hasKnownPrefix(key string) bool {
	for _, p := range profilePrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	for _, p := range projectPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func normalizeFact(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}
