CREATE TABLE IF NOT EXISTS records (
    id           TEXT PRIMARY KEY,
    scope_kind   TEXT NOT NULL CHECK (scope_kind IN ('profile','project')),
    project_id   TEXT NOT NULL DEFAULT '',
    key          TEXT NOT NULL,
    value        TEXT NOT NULL,
    confidence   REAL NOT NULL DEFAULT 0.5,
    klass        TEXT NOT NULL DEFAULT 'dynamic' CHECK (klass IN ('static','dynamic')),
    source       TEXT NOT NULL DEFAULT 'batch' CHECK (source IN ('batch','agent','user')),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','superseded','contradicted')),
    supersedes   TEXT NOT NULL DEFAULT '[]',
    contradicts  TEXT NOT NULL DEFAULT '[]',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_scope ON records (scope_kind, project_id, key, status);

CREATE TABLE IF NOT EXISTS docs (
    namespace  TEXT PRIMARY KEY,
    body       TEXT NOT NULL DEFAULT '',
    version    INTEGER NOT NULL DEFAULT 1,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    source        TEXT NOT NULL,
    session_id    TEXT NOT NULL,
    collector     TEXT NOT NULL DEFAULT '',
    project_hints TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL DEFAULT 0,
    ended_at      INTEGER NOT NULL DEFAULT 0,
    ingested_at   INTEGER NOT NULL,
    PRIMARY KEY (source, session_id)
);

CREATE TABLE IF NOT EXISTS turns (
    source     TEXT NOT NULL,
    session_id TEXT NOT NULL,
    idx        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    ts         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source, session_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns (source, session_id);

CREATE TABLE IF NOT EXISTS consolidated (
    source          TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    consolidated_at INTEGER NOT NULL,
    PRIMARY KEY (source, session_id)
);

CREATE TABLE IF NOT EXISTS watermarks (
    source TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS project_aliases (
    kind       TEXT NOT NULL CHECK (kind IN ('marker','remote','path','name')),
    value      TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (kind, value)
);
CREATE INDEX IF NOT EXISTS idx_aliases_project ON project_aliases (project_id);
