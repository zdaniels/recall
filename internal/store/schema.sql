PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY);
INSERT OR IGNORE INTO schema_version VALUES (1);

CREATE TABLE IF NOT EXISTS sources (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  kind             TEXT NOT NULL UNIQUE,
  path             TEXT NOT NULL,
  last_ingested_at INTEGER
);

CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,
  source_id       INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  project_dir     TEXT,
  git_branch      TEXT,
  source_version  TEXT,
  started_at      INTEGER,
  ended_at        INTEGER,
  summary         TEXT,
  turn_count      INTEGER NOT NULL DEFAULT 0,
  salience        REAL NOT NULL DEFAULT 1.0
);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_dir);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at);

CREATE TABLE IF NOT EXISTS turns (
  uuid          TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  parent_uuid   TEXT,
  idx           INTEGER NOT NULL,
  role          TEXT NOT NULL,
  ts            INTEGER,
  text          TEXT,
  has_tool_use  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_turns_session_idx ON turns(session_id, idx);
CREATE INDEX IF NOT EXISTS idx_turns_ts          ON turns(ts);

CREATE TABLE IF NOT EXISTS files (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  turn_uuid    TEXT,
  project_dir  TEXT,
  path         TEXT NOT NULL,
  op           TEXT NOT NULL,
  ts           INTEGER,
  UNIQUE(turn_uuid, path, op)
);
CREATE INDEX IF NOT EXISTS idx_files_path    ON files(path);
CREATE INDEX IF NOT EXISTS idx_files_project ON files(project_dir);

CREATE TABLE IF NOT EXISTS decisions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id    TEXT,
  project_dir   TEXT,
  ts            INTEGER,
  kind          TEXT,
  text          TEXT NOT NULL,
  source        TEXT NOT NULL,
  superseded_by INTEGER REFERENCES decisions(id),
  salience      REAL NOT NULL DEFAULT 1.0,
  use_count     INTEGER NOT NULL DEFAULT 0,
  last_used_at  INTEGER,
  confidence    REAL NOT NULL DEFAULT 0.5
);
CREATE INDEX IF NOT EXISTS idx_decisions_project ON decisions(project_dir);
CREATE INDEX IF NOT EXISTS idx_decisions_active  ON decisions(project_dir) WHERE superseded_by IS NULL;

CREATE TABLE IF NOT EXISTS decision_feedback (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  decision_id INTEGER NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  ts          INTEGER NOT NULL,
  weight      REAL NOT NULL DEFAULT 1.0,
  note        TEXT
);
CREATE INDEX IF NOT EXISTS idx_feedback_decision ON decision_feedback(decision_id);

CREATE TABLE IF NOT EXISTS edges (
  src_kind TEXT NOT NULL,
  src_id   TEXT NOT NULL,
  dst_kind TEXT NOT NULL,
  dst_id   TEXT NOT NULL,
  weight   REAL NOT NULL DEFAULT 1.0,
  PRIMARY KEY (src_kind, src_id, dst_kind, dst_id)
);

CREATE TABLE IF NOT EXISTS ingest_state (
  source_id   INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  key         TEXT NOT NULL,
  last_offset INTEGER NOT NULL DEFAULT 0,
  last_line   INTEGER NOT NULL DEFAULT 0,
  last_uuid   TEXT,
  updated_at  INTEGER,
  PRIMARY KEY (source_id, key)
);

CREATE VIRTUAL TABLE IF NOT EXISTS turn_fts USING fts5(
  text,
  content='turns',
  content_rowid='rowid',
  tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS turns_ai AFTER INSERT ON turns BEGIN
  INSERT INTO turn_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER IF NOT EXISTS turns_ad AFTER DELETE ON turns BEGIN
  INSERT INTO turn_fts(turn_fts, rowid, text) VALUES('delete', old.rowid, old.text);
END;
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE ON turns BEGIN
  INSERT INTO turn_fts(turn_fts, rowid, text) VALUES('delete', old.rowid, old.text);
  INSERT INTO turn_fts(rowid, text) VALUES (new.rowid, new.text);
END;
