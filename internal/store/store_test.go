package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/taciturnaxolotl/lard/internal/types"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSubjectRoundTrip(t *testing.T) {
	st := openTest(t)
	sub := &types.Subject{
		Kind:        types.KindArea,
		Name:        "crush",
		Description: "the crush TUI",
		Aliases:     []string{"Crush", "charmbracelet/crush"},
		ProjectID:   "p1",
		Body:        "- [stated] Open-source agentic coding TUI",
	}
	if err := st.PutSubject(sub, 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSubject(types.KindArea, "crush")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Body != sub.Body {
		t.Fatalf("body round-trip: %+v", got)
	}
	if got.ProjectID != "p1" || len(got.Aliases) != 2 {
		t.Fatalf("frontmatter round-trip: %+v", got)
	}
	// Alias resolution.
	name, err := st.ResolveSubject(types.KindArea, "charmbracelet/crush")
	if err != nil || name != "crush" {
		t.Fatalf("resolve by alias: %q %v", name, err)
	}
}

func TestFactsAndDirty(t *testing.T) {
	st := openTest(t)
	now := time.Now().UTC()
	facts := []types.Fact{
		{SubjectKind: types.KindTopic, SubjectName: "ctf-security", Text: "does CTFs", Tag: types.TagStated},
		{SubjectKind: types.KindTopic, SubjectName: "ctf-security", Text: "kernel exploits", Tag: types.TagStated},
	}
	if err := st.SaveFacts("crush", "sess-1", now, facts); err != nil {
		t.Fatal(err)
	}
	// Session should now be extracted (not re-listed).
	pending, err := st.ListUnextractedSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no unextracted sessions, got %d", len(pending))
	}
	// The subject should be dirty (facts newer than synthesis).
	dirty, err := st.DirtySubjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0] != [2]string{"topic", "ctf-security"} {
		t.Fatalf("expected ctf-security dirty, got %+v", dirty)
	}
	// Synthesize: fetch facts, write body with watermark, no longer dirty.
	fs, maxID, err := st.FactsForSubject(types.KindTopic, "ctf-security")
	if err != nil || len(fs) != 2 {
		t.Fatalf("facts: %d %v", len(fs), err)
	}
	sub := &types.Subject{Kind: types.KindTopic, Name: "ctf-security", Description: "security", Body: "- [stated] does CTFs, kernel exploits"}
	if err := st.PutSubject(sub, maxID); err != nil {
		t.Fatal(err)
	}
	dirty, _ = st.DirtySubjects()
	if len(dirty) != 0 {
		t.Fatalf("expected clean after synthesis, got %+v", dirty)
	}
}

func TestIngestIdempotent(t *testing.T) {
	st := openTest(t)
	batch := types.SessionBatch{
		SessionID: "sess-1", Source: "crush", StartedAt: "2026-07-26T10:00:00Z",
		Turns: []types.Turn{{Index: 0, Role: "user", Content: "hello", TS: "2026-07-26T10:00:00Z"}},
	}
	for i := 0; i < 3; i++ {
		if _, err := st.IngestSessions("test", []types.SessionBatch{batch}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := st.ListUnextractedSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(pending[0].Turns) != 1 {
		t.Fatalf("expected 1 session / 1 turn, got %d sessions", len(pending))
	}
}

func TestProjectRegistry(t *testing.T) {
	st := openTest(t)
	p, err := st.CreateProject("lard", map[string][]string{
		"remote": {"github.com/taciturnaxolotl/lard"},
		"path":   {"/Users/kierank/code/personal/lard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byRemote, err := st.FindProjectByAlias("remote", "github.com/taciturnaxolotl/lard")
	if err != nil || byRemote == nil || byRemote.ID != p.ID {
		t.Fatalf("find by remote: %v %v", byRemote, err)
	}
}
