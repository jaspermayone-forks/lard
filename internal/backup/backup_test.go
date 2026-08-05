package backup

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/tenant"
	"github.com/taciturnaxolotl/lard/internal/types"
)

func seed(t *testing.T, dbPath, memDir, subject string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath, memDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutSubject(&types.Subject{
		Kind: types.KindTopic, Name: subject, Body: "- a durable fact",
	}, 0); err != nil {
		t.Fatal(err)
	}
}

// readBack opens a backed-up store and returns its subject names, which is the
// real question: is the copy a usable database.
func readBack(t *testing.T, dbPath, memDir string) []string {
	t.Helper()
	st, err := store.Open(dbPath, memDir)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer st.Close()
	listing, err := st.ListSubjects()
	if err != nil {
		t.Fatalf("list from backup: %v", err)
	}
	var names []string
	for _, s := range listing {
		names = append(names, s.Name)
	}
	return names
}

func TestBackupSingleUser(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")
	seed(t, db, mem, "coffee")

	res, err := Run(dest, Options{DB: db, MemoryDir: mem})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("want 1 source, got %v", res.Sources)
	}
	if got := readBack(t, filepath.Join(dest, "lard.db"), filepath.Join(dest, "memory")); len(got) != 1 || got[0] != "coffee" {
		t.Fatalf("backup lost the subject: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "memory", "topics", "coffee.md")); err != nil {
		t.Fatalf("subject file not copied: %v", err)
	}
}

func TestBackupEveryTenant(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	layout := tenant.Layout{Root: live}
	for _, who := range []string{"https://alice.example", "https://bob.example"} {
		slug := tenant.Slug(who)
		seed(t, layout.DBPath(slug), layout.MemDir(slug), "notes-"+slug[:3])
	}

	res, err := Run(dest, Options{MultiUser: true, DataDir: live})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("want both tenants, got %v", res.Sources)
	}
	for _, who := range []string{"https://alice.example", "https://bob.example"} {
		slug := tenant.Slug(who)
		root := filepath.Join(dest, "users", slug)
		if got := readBack(t, filepath.Join(root, "lard.db"), filepath.Join(root, "memory")); len(got) != 1 {
			t.Fatalf("tenant %s: %v", slug, got)
		}
	}
}

// The claim that justifies not stopping the service: a snapshot taken while
// writes are landing is still a valid database. A plain file copy is what
// cannot promise this.
func TestSnapshotIsConsistentUnderConcurrentWrites(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")

	st, err := store.Open(db, mem)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutSubject(&types.Subject{Kind: types.KindTopic, Name: "seed", Body: "- one"}, 0); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = st.PutSubject(&types.Subject{
				Kind: types.KindTopic, Name: "churn", Body: "- write " + string(rune('a'+i%26)),
			}, 0)
		}
	})

	var lastErr error
	for i := range 5 {
		if _, err := Run(dest, Options{DB: db, MemoryDir: mem}); err != nil {
			lastErr = err
			break
		}
		if got := readBack(t, filepath.Join(dest, "lard.db"), filepath.Join(dest, "memory")); len(got) == 0 {
			lastErr = err
			t.Errorf("snapshot %d came back empty", i)
		}
	}
	close(stop)
	wg.Wait()
	if lastErr != nil {
		t.Fatalf("snapshot under load: %v", lastErr)
	}
}

func TestRestoreRoundTripSingleUser(t *testing.T) {
	live, dest, fresh := t.TempDir(), t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")
	seed(t, db, mem, "coffee")
	if _, err := Run(dest, Options{DB: db, MemoryDir: mem}); err != nil {
		t.Fatal(err)
	}

	// A restore onto a fresh machine: nothing in the way, no force needed.
	newDB, newMem := filepath.Join(fresh, "lard.db"), filepath.Join(fresh, "memory")
	if _, err := Restore(dest, Options{DB: newDB, MemoryDir: newMem}, false); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, newDB, newMem); len(got) != 1 || got[0] != "coffee" {
		t.Fatalf("round trip lost the subject: %v", got)
	}
}

func TestRestoreRoundTripMultiUser(t *testing.T) {
	live, dest, fresh := t.TempDir(), t.TempDir(), t.TempDir()
	layout := tenant.Layout{Root: live}
	slug := tenant.Slug("https://alice.example")
	seed(t, layout.DBPath(slug), layout.MemDir(slug), "coffee")
	if _, err := Run(dest, Options{MultiUser: true, DataDir: live}); err != nil {
		t.Fatal(err)
	}

	newLayout := tenant.Layout{Root: fresh}
	if _, err := Restore(dest, Options{MultiUser: true, DataDir: fresh}, false); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, newLayout.DBPath(slug), newLayout.MemDir(slug)); len(got) != 1 {
		t.Fatalf("tenant not restored: %v", got)
	}
}

func TestRestoreRefusesToClobberWithoutForce(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")
	seed(t, db, mem, "coffee")
	if _, err := Run(dest, Options{DB: db, MemoryDir: mem}); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(dest, Options{DB: db, MemoryDir: mem}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}

func TestRestoreKeepsWhatItReplaces(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")
	seed(t, db, mem, "coffee")
	if _, err := Run(dest, Options{DB: db, MemoryDir: mem}); err != nil {
		t.Fatal(err)
	}
	// The live store moves on, then gets rolled back over.
	seed(t, db, mem, "later-thought")

	res, err := Restore(dest, Options{DB: db, MemoryDir: mem}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MovedAside) == 0 {
		t.Fatal("force overwrote the previous data without keeping it")
	}
	for _, aside := range res.MovedAside {
		if _, err := os.Stat(aside); err != nil {
			t.Fatalf("kept path %s is not there: %v", aside, err)
		}
	}
	// And the restored store is the backup, not the newer state.
	if got := readBack(t, db, mem); len(got) != 1 || got[0] != "coffee" {
		t.Fatalf("want the backed-up state, got %v", got)
	}
}

func TestRestoreRefusesAShapeMismatch(t *testing.T) {
	live, dest := t.TempDir(), t.TempDir()
	db, mem := filepath.Join(live, "lard.db"), filepath.Join(live, "memory")
	seed(t, db, mem, "coffee")
	if _, err := Run(dest, Options{DB: db, MemoryDir: mem}); err != nil {
		t.Fatal(err)
	}
	// A single-user backup restored onto a multi-user server would land where
	// nothing ever reads it.
	if _, err := Restore(dest, Options{MultiUser: true, DataDir: t.TempDir()}, true); err == nil {
		t.Fatal("want a refusal on a shape mismatch, got nil")
	}
}

func TestRestoreRejectsSomethingThatIsNotABackup(t *testing.T) {
	if _, err := Restore(t.TempDir(), Options{DB: "x", MemoryDir: "y"}, true); err == nil {
		t.Fatal("want an error for a directory that is not a backup, got nil")
	}
}

func TestBackupWithNothingToCopy(t *testing.T) {
	if _, err := Run(t.TempDir(), Options{MultiUser: true, DataDir: t.TempDir()}); err == nil {
		t.Fatal("want an error when there is no data, got nil")
	}
}
