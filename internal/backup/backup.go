// Package backup takes a consistent, self-contained copy of lard's data while
// the server keeps running.
//
// The point is to let a backup tool work against a still directory instead of
// a live one. Databases are snapshotted with VACUUM INTO rather than copied,
// and subject files are copied plainly, which is safe because every write to
// them lands via a rename.
package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/tenant"
)

// Options mirrors the server's storage configuration.
type Options struct {
	MultiUser bool
	DB        string // single-user database
	MemoryDir string // single-user subject files
	DataDir   string // multi-user tenant root
}

// Source is one store to copy, and where it lands under the destination.
type Source struct {
	Name   string // tenant slug, or "" for a single-user server
	DB     string
	MemDir string
}

// Result reports what a run copied.
type Result struct {
	Sources []string
	Bytes   int64
	// MovedAside names any pre-existing data a restore preserved instead of
	// overwriting.
	MovedAside []string
}

// Plan lists the stores a backup must cover. The destination mirrors the live
// layout exactly, so restoring is a move rather than a puzzle.
func Plan(opts Options) []Source {
	if !opts.MultiUser {
		return []Source{{DB: opts.DB, MemDir: opts.MemoryDir}}
	}
	layout := tenant.Layout{Root: opts.DataDir}
	var out []Source
	for _, slug := range tenant.List(layout) {
		out = append(out, Source{Name: slug, DB: layout.DBPath(slug), MemDir: layout.MemDir(slug)})
	}
	return out
}

// Run copies every store into dest, laid out the way the live directory is.
func Run(dest string, opts Options) (Result, error) {
	var res Result
	sources := Plan(opts)
	if len(sources) == 0 {
		return res, fmt.Errorf("nothing to back up: no database at %s and no tenants under %s", opts.DB, opts.DataDir)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return res, err
	}
	for _, src := range sources {
		root := dest
		if src.Name != "" {
			root = filepath.Join(dest, "users", src.Name)
		}
		dbDest := filepath.Join(root, "lard.db")
		if err := store.Snapshot(src.DB, dbDest); err != nil {
			return res, err
		}
		if info, err := os.Stat(dbDest); err == nil {
			res.Bytes += info.Size()
		}
		n, err := copyTree(src.MemDir, filepath.Join(root, "memory"))
		if err != nil {
			return res, fmt.Errorf("copy %s: %w", src.MemDir, err)
		}
		res.Bytes += n

		name := src.Name
		if name == "" {
			name = "(single user)"
		}
		res.Sources = append(res.Sources, name)
	}
	return res, nil
}

// ErrExists means a restore would land on top of data that is already there.
var ErrExists = errors.New("destination already holds data")

// Restore puts a backup tree back where the server expects it.
//
// This exists because a backup tool restores the paths it archived, and what
// it archived was the staging directory, not the live one. Without this the
// last step of a restore is a hand-written mv at the worst possible moment.
//
// Existing data is never deleted: it is moved aside with a timestamped suffix,
// because the case where you are rolling back to last night is exactly the
// case where today's memory might still be wanted.
func Restore(src string, opts Options, force bool) (Result, error) {
	var res Result
	srcMulti, err := looksMultiUser(src)
	if err != nil {
		return res, err
	}
	if srcMulti != opts.MultiUser {
		return res, fmt.Errorf("backup at %s is %s but this server is configured %s",
			src, describe(srcMulti), describe(opts.MultiUser))
	}

	type move struct{ from, to string }
	var moves []move
	if !opts.MultiUser {
		moves = append(moves,
			move{filepath.Join(src, "lard.db"), opts.DB},
			move{filepath.Join(src, "memory"), opts.MemoryDir},
		)
	} else {
		layout := tenant.Layout{Root: opts.DataDir}
		entries, err := os.ReadDir(filepath.Join(src, "users"))
		if err != nil {
			return res, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			moves = append(moves, move{filepath.Join(src, "users", e.Name()), layout.Dir(e.Name())})
		}
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	for _, m := range moves {
		if _, err := os.Stat(m.from); err != nil {
			continue // a store with no subject files yet has no memory dir
		}
		if _, err := os.Stat(m.to); err == nil {
			if !force {
				return res, fmt.Errorf("%w at %s; pass --force to move it aside and restore over it", ErrExists, m.to)
			}
			aside := fmt.Sprintf("%s.superseded-%s", m.to, stamp)
			if err := os.Rename(m.to, aside); err != nil {
				return res, fmt.Errorf("move aside %s: %w", m.to, err)
			}
			res.MovedAside = append(res.MovedAside, aside)
		}
		if err := os.MkdirAll(filepath.Dir(m.to), 0o700); err != nil {
			return res, err
		}
		n, err := copyPath(m.from, m.to)
		if err != nil {
			return res, fmt.Errorf("restore %s: %w", m.from, err)
		}
		res.Bytes += n
		res.Sources = append(res.Sources, m.to)
	}
	if len(res.Sources) == 0 {
		return res, fmt.Errorf("nothing to restore from %s", src)
	}
	return res, nil
}

// looksMultiUser reads the shape of a backup tree rather than trusting the
// caller: restoring a tenant tree over a single-user server (or the reverse)
// would put the data somewhere nothing ever reads it.
func looksMultiUser(src string) (bool, error) {
	if _, err := os.Stat(filepath.Join(src, "users")); err == nil {
		return true, nil
	}
	if _, err := os.Stat(filepath.Join(src, "lard.db")); err == nil {
		return false, nil
	}
	return false, fmt.Errorf("%s does not look like a lard backup (no lard.db and no users/)", src)
}

func describe(multi bool) string {
	if multi {
		return "multi-user"
	}
	return "single-user"
}

func copyPath(src, dst string) (int64, error) {
	info, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return copyTree(src, dst)
	}
	return copyFile(src, dst)
}

// copyTree copies a directory recursively, reporting the bytes written. A
// missing source is not an error: a store that has never been consolidated has
// no subject files yet.
func copyTree(src, dst string) (int64, error) {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A live tree can lose an entry between the walk listing it and
			// this callback running. That is the rename finishing, not a
			// failure.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil // skip anything that is not a plain file
		}
		// Subject files are written to <name>.tmp and renamed into place, so a
		// .tmp here is a write in flight: half a file, and about to vanish.
		// The finished version either is already in the tree or will be in the
		// next backup.
		if strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		n, err := copyFile(path, target)
		if errors.Is(err, os.ErrNotExist) {
			return nil // lost the race to a rename; see above
		}
		total += n
		return err
	})
	return total, err
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		return n, err
	}
	return n, out.Sync()
}
