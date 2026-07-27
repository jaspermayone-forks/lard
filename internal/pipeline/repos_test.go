package pipeline

import (
	"slices"
	"testing"

	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir+"/lard.db", dir+"/memory")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRegistry(st)
}

func TestNormalizeRemotesCollapsesEquivalentForms(t *testing.T) {
	got := NormalizeRemotes([]string{
		"git@github.com:taciturnaxolotl/lard.git",
		"https://github.com/taciturnaxolotl/lard",
		"  ",
		"https://tangled.org/dunkirk.sh/lard/",
	})
	want := []string{"github.com/taciturnaxolotl/lard", "tangled.org/dunkirk.sh/lard"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormalizeRemotesPreservesOrder(t *testing.T) {
	got := NormalizeRemotes([]string{"tangled.org/a/b", "github.com/a/b"})
	if got[0] != "tangled.org/a/b" {
		t.Fatalf("first remote should stay canonical, got %v", got)
	}
}

// All mirrors of a project must resolve to one id. Resolving each separately
// would mint a project per mirror, which is the bug this guards.
func TestAttachReposLinksAllMirrorsToOneProject(t *testing.T) {
	reg := testRegistry(t)
	sub := &types.Subject{Kind: types.KindArea, Name: "lard"}
	repos := []string{
		"git@tangled.org:dunkirk.sh/lard.git",
		"https://github.com/taciturnaxolotl/lard",
	}
	if _, err := AttachRepos(reg, sub, repos); err != nil {
		t.Fatal(err)
	}
	if sub.ProjectID == "" {
		t.Fatal("area was not linked to a project")
	}
	if len(sub.Repos) != 2 {
		t.Fatalf("repos = %v, want 2", sub.Repos)
	}
	// Every mirror must now resolve back to the same project.
	for _, r := range sub.Repos {
		id, err := reg.Resolve(&types.ProjectHints{GitRemote: r})
		if err != nil {
			t.Fatal(err)
		}
		if id != sub.ProjectID {
			t.Errorf("remote %q resolved to %q, want %q", r, id, sub.ProjectID)
		}
	}
}

// A topic is not a project, so a repo on it is descriptive only.
func TestAttachReposDoesNotLinkNonAreas(t *testing.T) {
	reg := testRegistry(t)
	sub := &types.Subject{Kind: types.KindTopic, Name: "go"}
	if _, err := AttachRepos(reg, sub, []string{"github.com/golang/go"}); err != nil {
		t.Fatal(err)
	}
	if sub.ProjectID != "" {
		t.Errorf("topic should not be registry-linked, got %q", sub.ProjectID)
	}
	if len(sub.Repos) != 1 {
		t.Errorf("repos should still be recorded, got %v", sub.Repos)
	}
}

func TestAttachReposEmptyClearsWithoutLinking(t *testing.T) {
	reg := testRegistry(t)
	sub := &types.Subject{Kind: types.KindArea, Name: "lard"}
	if _, err := AttachRepos(reg, sub, []string{"", "   "}); err != nil {
		t.Fatal(err)
	}
	if len(sub.Repos) != 0 {
		t.Errorf("repos = %v, want empty", sub.Repos)
	}
	if sub.ProjectID != "" {
		t.Errorf("projectID = %q, want empty", sub.ProjectID)
	}
}

// Repos must survive the markdown round-trip, since the file is the source of
// truth for them rather than the sqlite index.
func TestReposRoundTripThroughFile(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/lard.db", dir+"/memory")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sub := &types.Subject{
		Kind:        types.KindArea,
		Name:        "lard",
		Description: "memory service",
		Repos:       []string{"tangled.org/dunkirk.sh/lard", "github.com/taciturnaxolotl/lard"},
		Body:        "- [stated] lard stores memory as markdown.",
	}
	if err := st.PutSubject(sub, 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSubject(types.KindArea, "lard")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("subject not found after write")
	}
	if !slices.Equal(got.Repos, sub.Repos) {
		t.Fatalf("repos = %v, want %v", got.Repos, sub.Repos)
	}
}
