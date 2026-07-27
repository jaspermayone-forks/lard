// Package store is lard's persistence layer. Subject files are markdown on
// disk (the human-facing, editable artifact); SQLite holds the machinery:
// ingested sessions and turns, extracted facts, the project registry, and a
// derived index of subject frontmatter for fast listing.
package store

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/google/uuid"

	"github.com/taciturnaxolotl/lard/internal/types"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store wraps the SQLite connection and the on-disk memory directory.
type Store struct {
	db     *sql.DB
	memDir string
}

// Open opens the database at dbPath and roots subject files at memDir.
func Open(dbPath, memDir string) (*Store, error) {
	params := url.Values{}
	params.Set("_pragma", "journal_mode(WAL)")
	params.Set("_pragma", "foreign_keys(ON)")
	params.Set("_pragma", "busy_timeout(30000)")
	params.Set("_txlock", "immediate")
	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, memDir: memDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	for _, sub := range []string{"areas", "topics", "people"} {
		if err := os.MkdirAll(filepath.Join(memDir, sub), 0o755); err != nil {
			db.Close()
			return nil, fmt.Errorf("create memory dir: %w", err)
		}
	}
	return s, nil
}

// Close closes the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

// MemDir returns the on-disk root of the subject files.
func (s *Store) MemDir() string { return s.memDir }

func (s *Store) migrate() error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("bootstrap schema_version: %w", err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &n); err != nil {
			continue
		}
		if n <= version {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, n); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- Sessions & turns (ingest) ---

// IngestSessions upserts session batches. Turns replace prior turns for
// (source, session_id): re-uploads of an open session are idempotent.
func (s *Store) IngestSessions(collector string, sessions []types.SessionBatch) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, sb := range sessions {
		hints, _ := json.Marshal(sb.ProjectHints)
		var started, ended int64
		if t, err := time.Parse(time.RFC3339, sb.StartedAt); err == nil {
			started = t.Unix()
		}
		if sb.EndedAt != "" {
			if t, err := time.Parse(time.RFC3339, sb.EndedAt); err == nil {
				ended = t.Unix()
			}
		}
		if _, err := tx.Exec(`INSERT INTO sessions
			(source, session_id, collector, project_hints, started_at, ended_at, ingested_at)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(source, session_id) DO UPDATE SET
				collector=excluded.collector, project_hints=excluded.project_hints,
				started_at=excluded.started_at, ended_at=excluded.ended_at, ingested_at=excluded.ingested_at`,
			sb.Source, sb.SessionID, collector, string(hints), started, ended, time.Now().UTC().Unix()); err != nil {
			return 0, fmt.Errorf("session %s: %w", sb.SessionID, err)
		}
		if _, err := tx.Exec(`DELETE FROM turns WHERE source = ? AND session_id = ?`, sb.Source, sb.SessionID); err != nil {
			return 0, err
		}
		for _, t := range sb.Turns {
			var ts int64
			if parsed, err := time.Parse(time.RFC3339, t.TS); err == nil {
				ts = parsed.Unix()
			}
			if _, err := tx.Exec(`INSERT INTO turns (source, session_id, idx, role, content, ts)
				VALUES (?,?,?,?,?,?)`, sb.Source, sb.SessionID, t.Index, t.Role, t.Content, ts); err != nil {
				return 0, fmt.Errorf("turn %s/%d: %w", sb.SessionID, t.Index, err)
			}
		}
		n++
	}
	return n, tx.Commit()
}

// PendingSession is a session awaiting extraction.
type PendingSession struct {
	Source    string
	SessionID string
	Hints     *types.ProjectHints
	StartedAt int64
	EndedAt   int64
	Turns     []types.Turn
}

// ListUnextractedSessions returns sessions not yet extracted, oldest first.
func (s *Store) ListUnextractedSessions(limit int) ([]*PendingSession, error) {
	rows, err := s.db.Query(`SELECT s.source, s.session_id, s.project_hints, s.started_at, s.ended_at
		FROM sessions s
		LEFT JOIN extracted e ON e.source = s.source AND e.session_id = s.session_id
		WHERE e.session_id IS NULL
		ORDER BY s.started_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []*PendingSession
	for rows.Next() {
		var p PendingSession
		var hints string
		if err := rows.Scan(&p.Source, &p.SessionID, &hints, &p.StartedAt, &p.EndedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if hints != "" && hints != "null" {
			_ = json.Unmarshal([]byte(hints), &p.Hints)
		}
		out = append(out, &p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range out {
		turns, err := s.turnsFor(p.Source, p.SessionID)
		if err != nil {
			return nil, err
		}
		p.Turns = turns
	}
	return out, nil
}

func (s *Store) turnsFor(source, sessionID string) ([]types.Turn, error) {
	rows, err := s.db.Query(`SELECT idx, role, content, ts FROM turns
		WHERE source = ? AND session_id = ? ORDER BY idx`, source, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.Turn
	for rows.Next() {
		var t types.Turn
		var ts int64
		if err := rows.Scan(&t.Index, &t.Role, &t.Content, &ts); err != nil {
			return nil, err
		}
		t.TS = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Facts (durable extraction output) ---

// SaveFacts persists a session's extracted facts and marks the session
// extracted, atomically. Re-extraction replaces prior facts for the session.
func (s *Store) SaveFacts(source, sessionID string, sessionDate time.Time, facts []types.Fact) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM facts WHERE source = ? AND session_id = ?`, source, sessionID); err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	for _, f := range facts {
		if _, err := tx.Exec(`INSERT INTO facts
			(source, session_id, subject_kind, subject_name, text, tag, sensitivity, session_date, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			source, sessionID, string(f.SubjectKind), f.SubjectName, f.Text, string(f.Tag),
			f.Sensitivity, sessionDate.UTC().Unix(), now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO extracted (source, session_id, extracted_at)
		VALUES (?,?,?) ON CONFLICT(source, session_id) DO UPDATE SET extracted_at=excluded.extracted_at`,
		source, sessionID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DirtySubjects returns (kind, name) pairs that have facts newer than the
// subject's last synthesis — i.e. subjects needing a re-synthesize.
func (s *Store) DirtySubjects() ([][2]string, error) {
	rows, err := s.db.Query(`
		SELECT f.subject_kind, f.subject_name
		FROM facts f
		LEFT JOIN subjects sub
		  ON sub.kind = f.subject_kind AND sub.name = f.subject_name
		GROUP BY f.subject_kind, f.subject_name
		HAVING COALESCE(sub.synth_max_fact_id, 0) < MAX(f.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out = append(out, [2]string{k, n})
	}
	return out, rows.Err()
}

// FactsForSubject returns all facts for a subject, oldest first, with the
// max fact id seen (the synthesis watermark to commit afterward).
func (s *Store) FactsForSubject(kind types.SubjectKind, name string) ([]types.Fact, int64, error) {
	rows, err := s.db.Query(`SELECT id, source, session_id, subject_kind, subject_name, text, tag, sensitivity, session_date, created_at
		FROM facts WHERE subject_kind = ? AND subject_name = ? ORDER BY session_date, id`, string(kind), name)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []types.Fact
	var maxID int64
	for rows.Next() {
		var f types.Fact
		var kk, tag string
		var sd, ca int64
		if err := rows.Scan(&f.ID, &f.Source, &f.SessionID, &kk, &f.SubjectName, &f.Text, &tag, &f.Sensitivity, &sd, &ca); err != nil {
			return nil, 0, err
		}
		f.SubjectKind = types.SubjectKind(kk)
		f.Tag = types.ProvenanceTag(tag)
		f.SessionDate = time.Unix(sd, 0).UTC()
		f.CreatedAt = time.Unix(ca, 0).UTC()
		out = append(out, f)
		if f.ID > maxID {
			maxID = f.ID
		}
	}
	return out, maxID, rows.Err()
}

// --- Subjects (markdown on disk + index) ---

// GetSubject loads a subject by (kind, name), reading its body from disk.
// Returns nil if the file does not exist.
func (s *Store) GetSubject(kind types.SubjectKind, name string) (*types.Subject, error) {
	path := types.SubjectPath(kind, name)
	body, err := os.ReadFile(filepath.Join(s.memDir, path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sub := parseSubject(kind, name, string(body))
	return sub, nil
}

// PutSubject writes a subject to disk (full overwrite) and refreshes its
// index row and synthesis watermark. synthFactID is the max fact id folded
// into this body (0 to leave the watermark unchanged).
func (s *Store) PutSubject(sub *types.Subject, synthFactID int64) error {
	sub.Updated = time.Now().UTC()
	content := renderSubjectFile(sub)
	sub.Version = hashBody(content)
	path := filepath.Join(s.memDir, sub.Path())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	aliases, _ := json.Marshal(sub.Aliases)
	_, err := s.db.Exec(`INSERT INTO subjects
		(kind, name, description, aliases, project_id, updated_at, synth_max_fact_id)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(kind, name) DO UPDATE SET
			description=excluded.description, aliases=excluded.aliases,
			project_id=excluded.project_id, updated_at=excluded.updated_at,
			synth_max_fact_id=MAX(subjects.synth_max_fact_id, excluded.synth_max_fact_id)`,
		string(sub.Kind), sub.Name, sub.Description, string(aliases), sub.ProjectID,
		sub.Updated.Unix(), synthFactID)
	return err
}

// ListSubjects returns the index (no bodies) for the listing surface.
func (s *Store) ListSubjects() ([]types.SubjectListing, error) {
	rows, err := s.db.Query(`SELECT kind, name, description, aliases, updated_at FROM subjects ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.SubjectListing
	for rows.Next() {
		var l types.SubjectListing
		var kind, aliases string
		var updated int64
		if err := rows.Scan(&kind, &l.Name, &l.Description, &aliases, &updated); err != nil {
			return nil, err
		}
		l.Kind = types.SubjectKind(kind)
		l.Path = types.SubjectPath(l.Kind, l.Name)
		l.Updated = time.Unix(updated, 0).UTC()
		_ = json.Unmarshal([]byte(aliases), &l.Aliases)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ResolveSubject finds an existing subject whose name or alias matches the
// given name (case-insensitive), within a kind. Returns "" if none.
func (s *Store) ResolveSubject(kind types.SubjectKind, name string) (string, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	rows, err := s.db.Query(`SELECT name, aliases FROM subjects WHERE kind = ?`, string(kind))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var n, aliases string
		if err := rows.Scan(&n, &aliases); err != nil {
			return "", err
		}
		if strings.ToLower(n) == want {
			return n, nil
		}
		var al []string
		_ = json.Unmarshal([]byte(aliases), &al)
		for _, a := range al {
			if strings.ToLower(a) == want {
				return n, nil
			}
		}
	}
	return "", rows.Err()
}

// SubjectForProject returns the name of the area linked to a project id, or
// "" if none is.
func (s *Store) SubjectForProject(projectID string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM subjects WHERE kind = ? AND project_id = ? LIMIT 1`,
		string(types.KindArea), projectID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// DeleteSubject removes a subject's file and index row.
func (s *Store) DeleteSubject(kind types.SubjectKind, name string) error {
	if err := os.Remove(filepath.Join(s.memDir, types.SubjectPath(kind, name))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM subjects WHERE kind = ? AND name = ?`, string(kind), name)
	return err
}

func hashBody(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// --- Project registry ---

func (s *Store) FindProjectByAlias(kind, value string) (*types.Project, error) {
	row := s.db.QueryRow(`SELECT p.id, p.display_name, p.created_at FROM projects p
		JOIN project_aliases a ON a.project_id = p.id
		WHERE a.kind = ? AND a.value = ? LIMIT 1`, kind, value)
	return s.scanProject(row)
}

func (s *Store) GetProject(id string) (*types.Project, error) {
	row := s.db.QueryRow(`SELECT id, display_name, created_at FROM projects WHERE id = ?`, id)
	return s.scanProject(row)
}

func (s *Store) scanProject(row *sql.Row) (*types.Project, error) {
	var p types.Project
	var created int64
	err := row.Scan(&p.ID, &p.DisplayName, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT kind, value FROM project_aliases WHERE project_id = ?`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return nil, err
		}
		switch kind {
		case "remote":
			p.Remotes = append(p.Remotes, value)
		case "path":
			p.Paths = append(p.Paths, value)
		case "name":
			p.Names = append(p.Names, value)
		}
	}
	return &p, rows.Err()
}

func (s *Store) CreateProject(displayName string, aliases map[string][]string) (*types.Project, error) {
	id := uuid.NewString()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO projects (id, display_name, created_at) VALUES (?,?,?)`,
		id, displayName, time.Now().UTC().Unix()); err != nil {
		return nil, err
	}
	for kind, values := range aliases {
		for _, v := range values {
			if v == "" {
				continue
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO project_aliases (kind, value, project_id) VALUES (?,?,?)`,
				kind, v, id); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetProject(id)
}

func (s *Store) AddAlias(projectID, kind, value string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO project_aliases (kind, value, project_id) VALUES (?,?,?)`,
		kind, value, projectID)
	return err
}

func (s *Store) ListProjects() ([]*types.Project, error) {
	rows, err := s.db.Query(`SELECT id FROM projects ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*types.Project
	for _, id := range ids {
		p, err := s.GetProject(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
