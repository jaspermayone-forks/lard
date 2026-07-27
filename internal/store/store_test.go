package store

import (
	"path/filepath"
	"testing"

	"github.com/taciturnaxolotl/lard/internal/types"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRecordRoundTrip(t *testing.T) {
	st := openTest(t)
	r := &types.Record{
		Scope:      types.Scope{Kind: types.ScopeProject, ProjectID: "p1"},
		Key:        "conventions.pkg-manager",
		Value:      "uses pnpm",
		Confidence: 0.9,
		Class:      types.ClassStatic,
		Source:     types.SourceBatch,
		Status:     types.StatusActive,
	}
	if err := st.UpsertRecord(r); err != nil {
		t.Fatal(err)
	}
	if r.ID == "" {
		t.Fatal("expected id to be assigned")
	}
	recs, err := st.ListRecords("project", "p1", "conventions.pkg-manager", "active")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Value != "uses pnpm" {
		t.Fatalf("got %+v", recs)
	}
}

func TestIngestIdempotent(t *testing.T) {
	st := openTest(t)
	batch := types.SessionBatch{
		SessionID: "sess-1",
		Source:    "crush",
		StartedAt: "2026-07-26T10:00:00Z",
		Turns: []types.Turn{
			{Index: 0, Role: "user", Content: "hello", TS: "2026-07-26T10:00:00Z"},
		},
	}
	for i := 0; i < 3; i++ {
		if _, err := st.IngestSessions("test", []types.SessionBatch{batch}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := st.ListPendingSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending session after re-ingest, got %d", len(pending))
	}
	if len(pending[0].Turns) != 1 {
		t.Fatalf("expected 1 turn after idempotent re-ingest, got %d", len(pending[0].Turns))
	}
	// Growing session: re-upload with an appended turn replaces the old set.
	batch.Turns = append(batch.Turns, types.Turn{Index: 1, Role: "user", Content: "more", TS: "2026-07-26T10:05:00Z"})
	if _, err := st.IngestSessions("test", []types.SessionBatch{batch}); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.ListPendingSessions(10)
	if len(pending[0].Turns) != 2 {
		t.Fatalf("expected 2 turns after append, got %d", len(pending[0].Turns))
	}
	if err := st.MarkConsolidated("crush", "sess-1"); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.ListPendingSessions(10)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after mark consolidated, got %d", len(pending))
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
	// Find by each alias kind.
	byRemote, err := st.FindProjectByAlias("remote", "github.com/taciturnaxolotl/lard")
	if err != nil || byRemote == nil || byRemote.ID != p.ID {
		t.Fatalf("find by remote: %v %v", byRemote, err)
	}
	byPath, _ := st.FindProjectByAlias("path", "/Users/kierank/code/personal/lard")
	if byPath == nil || byPath.ID != p.ID {
		t.Fatalf("find by path failed")
	}
	// Merge: second project's aliases and records fold into the first.
	q, _ := st.CreateProject("lard-laptop", map[string][]string{
		"path": {"/home/kieran/lard"},
	})
	if q == nil {
		t.Fatal("create second project failed")
	}
	rec := &types.Record{
		Scope: types.Scope{Kind: types.ScopeProject, ProjectID: q.ID},
		Key:   "decisions.db", Value: "chose sqlite", Source: types.SourceBatch, Status: types.StatusActive,
	}
	if err := st.UpsertRecord(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeProjects(p.ID, q.ID); err != nil {
		t.Fatal(err)
	}
	merged, _ := st.FindProjectByAlias("path", "/home/kieran/lard")
	if merged == nil || merged.ID != p.ID {
		t.Fatalf("alias did not move on merge")
	}
	recs, _ := st.ListRecords("project", p.ID, "", "active")
	if len(recs) != 1 || recs[0].Value != "chose sqlite" {
		t.Fatalf("records did not repoint on merge: %+v", recs)
	}
	gone, _ := st.GetProject(q.ID)
	if gone != nil {
		t.Fatalf("merged-from project still exists")
	}
}

func TestDocsAndContradictions(t *testing.T) {
	st := openTest(t)
	if err := st.PutDoc("profile/preferences", "# Preferences\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDoc("profile/preferences", "# Preferences v2\n"); err != nil {
		t.Fatal(err)
	}
	body, _ := st.GetDoc("profile/preferences")
	if body != "# Preferences v2\n" {
		t.Fatalf("doc upsert: %q", body)
	}
	a := &types.Record{Scope: types.Scope{Kind: types.ScopeProfile}, Key: "comms.style", Value: "terse", Source: types.SourceBatch, Status: types.StatusContradicted}
	b := &types.Record{Scope: types.Scope{Kind: types.ScopeProfile}, Key: "comms.style", Value: "detailed", Source: types.SourceBatch, Status: types.StatusContradicted}
	_ = st.UpsertRecord(a)
	b.Contradicts = []string{a.ID}
	_ = st.UpsertRecord(b)
	pairs, err := st.ListContradictions()
	if err != nil || len(pairs) != 1 {
		t.Fatalf("contradictions: %v %v", pairs, err)
	}
}
