// Package store is lard's SQLite persistence layer: records, rendered
// documents, ingested turns, the project registry, and watermarks.
package store

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/google/uuid"

	"github.com/taciturnaxolotl/lard/internal/types"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	params := url.Values{}
	params.Set("_pragma", "journal_mode(WAL)")
	params.Set("_pragma", "foreign_keys(ON)")
	params.Set("_pragma", "busy_timeout(30000)")
	params.Set("_txlock", "immediate")
	dsn := fmt.Sprintf("file:%s?%s", path, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var version int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&version); err != nil {
		// schema_version may not exist yet; bootstrap below.
		if _, execErr := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); execErr != nil {
			return fmt.Errorf("bootstrap schema_version: %w", execErr)
		}
		version = 0
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

func scopeKey(sc types.Scope) (kind, project string) {
	return string(sc.Kind), sc.ProjectID
}

// UpsertRecord inserts or replaces a record.
func (s *Store) UpsertRecord(r *types.Record) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	if r.LastSeenAt.IsZero() {
		r.LastSeenAt = now
	}
	sup, _ := json.Marshal(r.Supersedes)
	con, _ := json.Marshal(r.Contradicts)
	kind, project := scopeKey(r.Scope)
	// Tolerate zero-value fields from direct construction; the schema has
	// CHECK constraints and the zero values are never valid.
	if kind == "" {
		kind = string(types.ScopeProfile)
	}
	if r.Class == "" {
		r.Class = types.ClassDynamic
	}
	if r.Source == "" {
		r.Source = types.SourceBatch
	}
	if r.Status == "" {
		r.Status = types.StatusActive
	}
	if r.Confidence == 0 {
		r.Confidence = 0.5
	}
	_, err := s.db.Exec(`INSERT INTO records
		(id, scope_kind, project_id, key, value, confidence, klass, source, status, supersedes, contradicts, created_at, updated_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			value=excluded.value, confidence=excluded.confidence, klass=excluded.klass,
			source=excluded.source, status=excluded.status, supersedes=excluded.supersedes,
			contradicts=excluded.contradicts, updated_at=excluded.updated_at, last_seen_at=excluded.last_seen_at`,
		r.ID, kind, project, r.Key, r.Value, r.Confidence, string(r.Class), string(r.Source), string(r.Status),
		string(sup), string(con), r.CreatedAt.Unix(), r.UpdatedAt.Unix(), r.LastSeenAt.Unix())
	return err
}

var recordCols = `id, scope_kind, project_id, key, value, confidence, klass, source, status, supersedes, contradicts, created_at, updated_at, last_seen_at`

func scanRecord(row interface{ Scan(...any) error }) (*types.Record, error) {
	var r types.Record
	var kind, klass, source, status, sup, con string
	var created, updated, seen int64
	err := row.Scan(&r.ID, &kind, &r.Scope.ProjectID, &r.Key, &r.Value, &r.Confidence,
		&klass, &source, &status, &sup, &con, &created, &updated, &seen)
	if err != nil {
		return nil, err
	}
	r.Scope.Kind = types.ScopeKind(kind)
	r.Class = types.RecordClass(klass)
	r.Source = types.RecordSource(source)
	r.Status = types.RecordStatus(status)
	_ = json.Unmarshal([]byte(sup), &r.Supersedes)
	_ = json.Unmarshal([]byte(con), &r.Contradicts)
	r.CreatedAt = time.Unix(created, 0).UTC()
	r.UpdatedAt = time.Unix(updated, 0).UTC()
	r.LastSeenAt = time.Unix(seen, 0).UTC()
	return &r, nil
}

// ListRecords returns records matching the filters. Empty scopeKind matches all.
// status empty means "active".
func (s *Store) ListRecords(scopeKind, projectID, key, status string) ([]*types.Record, error) {
	if status == "" {
		status = string(types.StatusActive)
	}
	q := `SELECT ` + recordCols + ` FROM records WHERE 1=1`
	var args []any
	if status != "*" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if scopeKind != "" {
		q += ` AND scope_kind = ?`
		args = append(args, scopeKind)
	}
	if projectID != "" {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if key != "" {
		// Prefix match: "preferences.editor" also gathers
		// "preferences.editor.vim-exrc" so near-identical keys cluster for
		// reconciliation instead of fragmenting.
		q += ` AND (key = ? OR key LIKE ?)`
		args = append(args, key, key+".%")
	}
	q += ` ORDER BY key, confidence DESC, updated_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*types.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRecord fetches one record by id.
func (s *Store) GetRecord(id string) (*types.Record, error) {
	row := s.db.QueryRow(`SELECT `+recordCols+` FROM records WHERE id = ?`, id)
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// SoftDeleteKey marks active records at (scope, key) as superseded.
func (s *Store) SoftDeleteKey(scope types.Scope, key string) (int64, error) {
	kind, project := scopeKey(scope)
	res, err := s.db.Exec(`UPDATE records SET status = ?, updated_at = ?
		WHERE scope_kind = ? AND project_id = ? AND key = ? AND status = ?`,
		string(types.StatusSuperseded), time.Now().UTC().Unix(), kind, project, key, string(types.StatusActive))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PutDoc upserts a rendered document for a namespace (e.g. "profile/preferences").
func (s *Store) PutDoc(namespace, body string) error {
	_, err := s.db.Exec(`INSERT INTO docs (namespace, body, version, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(namespace) DO UPDATE SET body=excluded.body, version=docs.version+1, updated_at=excluded.updated_at`,
		namespace, body, time.Now().UTC().Unix())
	return err
}

// GetDoc fetches a rendered document. Returns "" if absent.
func (s *Store) GetDoc(namespace string) (string, error) {
	var body string
	err := s.db.QueryRow(`SELECT body FROM docs WHERE namespace = ?`, namespace).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body, err
}

// ListDocNamespaces lists namespaces under a prefix ("profile" or "project/<id>").
func (s *Store) ListDocNamespaces(prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT namespace FROM docs WHERE namespace = ? OR namespace LIKE ? ORDER BY namespace`,
		prefix, prefix+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

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

// PendingSession is a session awaiting consolidation.
type PendingSession struct {
	Source    string
	SessionID string
	Hints     *types.ProjectHints
	StartedAt int64
	EndedAt   int64
	Turns     []types.Turn
}

// ListPendingSessions returns sessions not yet marked consolidated.
func (s *Store) ListPendingSessions(limit int) ([]*PendingSession, error) {
	rows, err := s.db.Query(`SELECT s.source, s.session_id, s.project_hints, s.started_at, s.ended_at
		FROM sessions s
		LEFT JOIN consolidated c ON c.source = s.source AND c.session_id = s.session_id
		WHERE c.session_id IS NULL
		ORDER BY s.started_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	// Buffer the session rows before querying turns: the connection pool is
	// capped at one, so nesting a second query inside the iterator deadlocks.
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

// MarkConsolidated records (source, sessionID) as processed.
func (s *Store) MarkConsolidated(source, sessionID string) error {
	_, err := s.db.Exec(`INSERT INTO consolidated (source, session_id, consolidated_at)
		VALUES (?,?,?) ON CONFLICT(source, session_id) DO NOTHING`,
		source, sessionID, time.Now().UTC().Unix())
	return err
}

// GetWatermark returns the per-source offset (opaque to the store).
func (s *Store) GetWatermark(source string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM watermarks WHERE source = ?`, source).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetWatermark stores the per-source offset.
func (s *Store) SetWatermark(source, value string) error {
	_, err := s.db.Exec(`INSERT INTO watermarks (source, value) VALUES (?,?)
		ON CONFLICT(source) DO UPDATE SET value=excluded.value`, source, value)
	return err
}

// Project registry

// FindProjectByAlias looks up a project by (kind, value) alias.
// kind is one of "remote", "path", "name".
func (s *Store) FindProjectByAlias(kind, value string) (*types.Project, error) {
	row := s.db.QueryRow(`SELECT p.id, p.display_name, p.created_at FROM projects p
		JOIN project_aliases a ON a.project_id = p.id
		WHERE a.kind = ? AND a.value = ? LIMIT 1`, kind, value)
	return s.scanProject(row)
}

// GetProject fetches a project by id.
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

// CreateProject mints a project and seeds it with the given aliases.
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

// AddAlias binds another alias to a project.
func (s *Store) AddAlias(projectID, kind, value string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO project_aliases (kind, value, project_id) VALUES (?,?,?)`,
		kind, value, projectID)
	return err
}

// ListProjects returns all registered projects.
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

// MergeProjects moves all aliases and records from `fromID` into `intoID`, then deletes `fromID`.
func (s *Store) MergeProjects(intoID, fromID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE OR IGNORE project_aliases SET project_id = ? WHERE project_id = ?`, intoID, fromID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_aliases WHERE project_id = ?`, fromID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE records SET project_id = ? WHERE project_id = ?`, intoID, fromID); err != nil {
		return err
	}
	// Re-namespace docs from the old project into the new one.
	rows, err := tx.Query(`SELECT namespace, body FROM docs WHERE namespace LIKE ?`, "project/"+fromID+"/%")
	if err != nil {
		return err
	}
	type doc struct{ ns, body string }
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.ns, &d.body); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	for _, d := range docs {
		newNS := strings.Replace(d.ns, "project/"+fromID+"/", "project/"+intoID+"/", 1)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO docs (namespace, body, version, updated_at) VALUES (?,?,1,?)`,
			newNS, d.body, time.Now().UTC().Unix()); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM docs WHERE namespace = ?`, d.ns); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE id = ?`, fromID); err != nil {
		return err
	}
	return tx.Commit()
}

// ContradictionPair is an unresolved tension between two active records.
type ContradictionPair struct {
	A *types.Record `json:"a"`
	B *types.Record `json:"b"`
}

// ListContradictions returns pairs of records linked by a contradicts edge,
// whether active or already flagged contradicted.
func (s *Store) ListContradictions() ([]ContradictionPair, error) {
	recs, err := s.ListRecords("", "", "", "*")
	if err != nil {
		return nil, err
	}
	// Only pairs still standing count: drop anything already superseded.
	var live []*types.Record
	for _, r := range recs {
		if r.Status != types.StatusSuperseded {
			live = append(live, r)
		}
	}
	recs = live
	byID := map[string]*types.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	seen := map[string]bool{}
	var out []ContradictionPair
	for _, r := range recs {
		for _, other := range r.Contradicts {
			key := r.ID + "|" + other
			rev := other + "|" + r.ID
			if seen[key] || seen[rev] {
				continue
			}
			if o, ok := byID[other]; ok {
				out = append(out, ContradictionPair{A: r, B: o})
				seen[key] = true
			}
		}
	}
	return out, nil
}
