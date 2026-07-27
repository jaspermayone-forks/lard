-- Subject-file model: facts as durable extraction output, subjects as an
-- index over on-disk markdown files, sessions/turns unchanged, extracted
-- replaces the old consolidated marker.

CREATE TABLE IF NOT EXISTS facts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    source       TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    subject_kind TEXT NOT NULL,
    subject_name TEXT NOT NULL,
    text         TEXT NOT NULL,
    tag          TEXT NOT NULL DEFAULT 'stated',
    sensitivity  TEXT NOT NULL DEFAULT '',
    session_date INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_facts_subject ON facts (subject_kind, subject_name);
CREATE INDEX IF NOT EXISTS idx_facts_session ON facts (source, session_id);

CREATE TABLE IF NOT EXISTS subjects (
    kind              TEXT NOT NULL,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    aliases           TEXT NOT NULL DEFAULT '[]',
    project_id        TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL,
    synth_max_fact_id INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, name)
);

CREATE TABLE IF NOT EXISTS extracted (
    source       TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    extracted_at INTEGER NOT NULL,
    PRIMARY KEY (source, session_id)
);
