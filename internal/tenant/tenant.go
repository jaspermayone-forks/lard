// Package tenant lays out per-user storage. A tenant is a whole store: its
// own SQLite database and its own directory of markdown subject files. Nothing
// is shared, so no query in the store layer needs to know users exist.
//
// The alternative — a user_id column on every table — would still need a
// directory per user for the subject files, and would put the burden of
// remembering the filter on every future query. Isolation by directory is the
// version that cannot be forgotten.
package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/auth"
	"github.com/taciturnaxolotl/lard/internal/store"
)

// Layout maps tenant keys to paths under a single data root.
type Layout struct{ Root string }

// Slug turns an identity (an authorization server "me" URL) into a directory
// name: a readable prefix for whoever is reading `ls`, plus a hash so that two
// identities never collide and nothing from the URL can escape the root.
//
// The identity is normalized first, on auth's definition of sameness. A
// trailing slash appearing or vanishing between two tokens must not hand the
// same person a second, empty memory.
func Slug(subject string) string {
	subject = auth.NormalizeSubject(subject)
	sum := sha256.Sum256([]byte(subject))
	digest := hex.EncodeToString(sum[:])[:12]

	var b strings.Builder
	last := byte('-')
	for i := 0; i < len(subject) && b.Len() < 32; i++ {
		c := subject[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
			fallthrough
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			last = c
		default:
			if last != '-' {
				b.WriteByte('-')
				last = '-'
			}
		}
	}
	readable := strings.Trim(b.String(), "-")
	// Scheme prefixes are noise on every identity from the same provider.
	for _, p := range []string{"https-", "http-"} {
		readable = strings.TrimPrefix(readable, p)
	}
	if readable == "" {
		readable = "user"
	}
	return readable + "-" + digest
}

// Dir is a tenant's directory.
func (l Layout) Dir(slug string) string { return filepath.Join(l.Root, slug) }

// DBPath is a tenant's SQLite database.
func (l Layout) DBPath(slug string) string { return filepath.Join(l.Dir(slug), "lard.db") }

// MemDir is a tenant's subject-file root.
func (l Layout) MemDir(slug string) string { return filepath.Join(l.Dir(slug), "memory") }

// Open opens a tenant's store, creating its directory on first contact.
func (l Layout) Open(slug string) (*store.Store, error) {
	if err := os.MkdirAll(l.Dir(slug), 0o700); err != nil {
		return nil, fmt.Errorf("create tenant dir: %w", err)
	}
	st, err := store.Open(l.DBPath(slug), l.MemDir(slug))
	if err != nil {
		return nil, fmt.Errorf("open tenant %s: %w", slug, err)
	}
	return st, nil
}

// Exists reports whether a tenant's directory has been created.
func (l Layout) Exists(slug string) bool {
	_, err := os.Stat(l.Dir(slug))
	return err == nil
}

// isSlug reports whether a directory name is one Slug could have produced:
// something readable, then a dash, then twelve hex digits.
//
// Holding directories to that shape matters because the data root collects
// company over time. A restore parks the data it replaced next to the tenant
// it replaced, and that copy has a lard.db in it; without this check it would
// be counted as a tenant, backed up on every run, and quietly doubled forever.
func isSlug(name string) bool {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return false
	}
	digest := name[i+1:]
	if len(digest) != 12 {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// List names the tenants that already have a directory under the root.
func List(l Layout) []string {
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !isSlug(e.Name()) {
			continue
		}
		if _, err := os.Stat(l.DBPath(e.Name())); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// AdoptLegacy moves a single-user database and memory directory into a
// tenant's directory, so turning on multi-user does not strand the memory
// accumulated before the switch. It reports whether it moved anything, and
// does nothing if the tenant directory already exists.
func AdoptLegacy(l Layout, slug, legacyDB, legacyMem string) (bool, error) {
	if slug == "" || l.Exists(slug) {
		return false, nil
	}
	if _, err := os.Stat(legacyDB); err != nil {
		return false, nil
	}
	if err := os.MkdirAll(l.Dir(slug), 0o700); err != nil {
		return false, err
	}
	// The WAL and shared-memory sidecars belong to the database. Moving the
	// database alone would leave committed-but-uncheckpointed transactions
	// behind, which is data loss dressed up as a successful migration.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := legacyDB + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, l.DBPath(slug)+suffix); err != nil {
			return false, fmt.Errorf("move %s: %w", src, err)
		}
	}
	if _, err := os.Stat(legacyMem); err == nil {
		if err := os.Rename(legacyMem, l.MemDir(slug)); err != nil {
			return false, fmt.Errorf("move %s: %w", legacyMem, err)
		}
	}
	return true, nil
}
