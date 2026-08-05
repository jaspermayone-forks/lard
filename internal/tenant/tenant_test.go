package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugIsReadableAndStable(t *testing.T) {
	got := Slug("https://dunkirk.sh/")
	if !strings.HasPrefix(got, "dunkirk-sh-") {
		t.Fatalf("want a readable prefix, got %q", got)
	}
	if got != Slug("https://dunkirk.sh/") {
		t.Fatal("slug is not stable across calls")
	}
}

func TestSlugFollowsAuthsDefinitionOfSameness(t *testing.T) {
	// The allowlist accepts all four of these as one person, so they must all
	// open the same memory. A trailing slash showing up on a refreshed token
	// must not silently hand the user an empty tenant.
	want := Slug("https://dunkirk.sh/")
	for _, variant := range []string{"https://dunkirk.sh", "https://Dunkirk.sh/", " https://dunkirk.sh/ "} {
		if got := Slug(variant); got != want {
			t.Fatalf("%q slugged to %q, want %q", variant, got, want)
		}
	}
}

func TestSlugSeparatesDistinctIdentities(t *testing.T) {
	// Same readable prefix after filtering, different identities: the hash is
	// the only thing keeping these two people's memory apart.
	a := Slug("https://example.com/a")
	b := Slug("https://example.com/b")
	if a == b {
		t.Fatalf("distinct identities collided: %q", a)
	}
}

func TestSlugCannotEscapeTheRoot(t *testing.T) {
	for _, subject := range []string{"../../etc/passwd", "https://x/../../y", "/", ""} {
		s := Slug(subject)
		if s != filepath.Base(s) || strings.Contains(s, "..") {
			t.Fatalf("subject %q produced traversable slug %q", subject, s)
		}
	}
}

func TestListSkipsWhatIsNotATenant(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	slug := Slug("https://alice.example")
	// A real tenant, and the copy a forced restore parks beside it. Both hold
	// a lard.db; only one is a tenant.
	for _, dir := range []string{slug, slug + ".superseded-20260805-160658", "notes", "lost+found"} {
		if err := os.MkdirAll(l.Dir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.DBPath(dir), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := List(l)
	if len(got) != 1 || got[0] != slug {
		t.Fatalf("want just %q, got %v", slug, got)
	}
}

func TestAdoptLegacyMovesDatabaseAndSidecars(t *testing.T) {
	dir := t.TempDir()
	legacyDB := filepath.Join(dir, "lard.db")
	legacyMem := filepath.Join(dir, "memory")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(legacyDB+suffix, []byte(suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(legacyMem, "areas"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := Layout{Root: filepath.Join(dir, "users")}
	moved, err := AdoptLegacy(l, "krn-abc", legacyDB, legacyMem)
	if err != nil || !moved {
		t.Fatalf("adopt: moved=%v err=%v", moved, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(l.DBPath("krn-abc") + suffix); err != nil {
			t.Fatalf("sidecar %q not moved: %v", suffix, err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.MemDir("krn-abc"), "areas")); err != nil {
		t.Fatalf("memory dir not moved: %v", err)
	}

	// Idempotent: a second boot must not clobber the tenant it just made.
	if moved, err := AdoptLegacy(l, "krn-abc", legacyDB, legacyMem); err != nil || moved {
		t.Fatalf("second adopt: moved=%v err=%v", moved, err)
	}
}

func TestAdoptLegacyWithNothingToAdopt(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	moved, err := AdoptLegacy(l, "krn-abc", filepath.Join(t.TempDir(), "absent.db"), "")
	if err != nil || moved {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
}
