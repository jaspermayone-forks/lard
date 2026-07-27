package pipeline

import (
	"testing"

	"github.com/taciturnaxolotl/lard/internal/types"
)

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:org/repo.git":        "github.com/org/repo",
		"https://github.com/org/repo":        "github.com/org/repo",
		"https://github.com/org/repo.git":    "github.com/org/repo",
		"https://github.com/org/repo/":       "github.com/org/repo",
		"ssh://git@github.com/org/repo.git":  "github.com/org/repo",
		"https://user@GitHub.com/Org/Repo":   "github.com/Org/Repo",
		"git@gitlab.com:group/sub/repo.git":  "gitlab.com/group/sub/repo",
		"github.com/org/repo":                "github.com/org/repo",
		"":                                   "",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGateRouting(t *testing.T) {
	cands := []types.Candidate{
		{Fact: "prefers Go", Key: "preferences.language", ScopeHint: "project", Confidence: 0.9},
		{Fact: "uses pnpm", Key: "conventions.pkg-manager", ScopeHint: "profile", Confidence: 0.8},
		{Fact: "uses App Router", Key: "stack.routing", Confidence: 0.5},
		{Fact: "has back pain", Key: "identity.health", Sensitivity: strPtr("health"), Confidence: 0.9},
		{Fact: "prefers Go", Key: "preferences.language", Confidence: 0.6}, // dupe, lower conf
	}
	out := Gate(cands, "proj-123", "")
	if len(out) != 3 {
		t.Fatalf("expected 3 candidates after gate, got %d: %+v", len(out), out)
	}
	byKey := map[string]GatedCandidate{}
	for _, g := range out {
		byKey[g.Candidate.Key] = g
	}
	// Profile prefix wins over project hint.
	if g := byKey["preferences.language"]; g.Scope.Kind != types.ScopeProfile {
		t.Errorf("preferences.language routed to %v, want profile", g.Scope)
	} else if g.Candidate.Confidence != 0.9 {
		t.Errorf("dedupe kept confidence %v, want 0.9", g.Candidate.Confidence)
	}
	// Project prefix stays project even with profile hint.
	if g := byKey["conventions.pkg-manager"]; g.Scope.Kind != types.ScopeProject || g.Scope.ProjectID != "proj-123" {
		t.Errorf("conventions.pkg-manager routed to %+v, want project/proj-123", g.Scope)
	}
	// Unknown key with no hint follows origin.
	if g := byKey["stack.routing"]; g.Scope.Kind != types.ScopeProject {
		t.Errorf("stack.routing routed to %+v, want project (origin)", g.Scope)
	}
}

func TestRouteNoProject(t *testing.T) {
	// Project-prefixed keys with no project context fall back to profile.
	if s := Route("conventions.build", "project", ""); s.Kind != types.ScopeProfile {
		t.Errorf("Route with no project = %+v, want profile fallback", s)
	}
}

func strPtr(s string) *string { return &s }
