// Package store wraps the SQLite backing database for recall.
//
// Single source of truth for the schema (embedded schema.sql), plus typed
// Open/Tx/upsert primitives consumed by adapters. Open() applies the schema
// idempotently and refuses to start against a future schema version.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var _ = errors.Is // silence; used by some callers via store.ErrNoRows etc.

//go:embed schema.sql
var schemaSQL string

const currentSchemaVersion = 9

// migrations holds incremental upgrades applied on top of the base schema.
// v1 base; v2 vec + tool_events; v3 summary_consolidated_at; v4 pins;
// v5 decision_paraphrases for HyDE-adjacent multi-vector search;
// v6 entities + entity_mentions for @entity-scoped retrieval;
// v7 distilled_at on turns for incremental `recall distill` runs.
// v8 confidence column + decision_feedback log for trust scoring.
// v9 entity_cards: auto-curated per-entity wiki summaries.
var migrations = []struct {
	version int
	apply   func(db *sql.DB) error
}{
	{2, applyV2},
	{3, applyV3},
	{4, applyV4},
	{5, applyV5},
	{6, applyV6},
	{7, applyV7},
	{8, applyV8},
	{9, applyV9},
}

// applyV9 adds the entity wiki: one rolled-up, LLM-distilled "card" per
// entity, refreshed as new mentions accumulate. Keyed by entity_id (one card
// per entity); the embedding lets cards surface in semantic recall.
func applyV9(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS entity_cards (
		  entity_id    INTEGER PRIMARY KEY REFERENCES entities(id) ON DELETE CASCADE,
		  summary      TEXT NOT NULL,
		  embedding    BLOB,
		  source_count INTEGER NOT NULL DEFAULT 0,
		  refreshed_at INTEGER NOT NULL
		);
	`); err != nil {
		return err
	}
	return nil
}

// applyV8 adds trust/confidence scoring. `confidence` is a [0,1] score that
// rises when a fact is corroborated (a near-duplicate is re-asserted) and
// falls when it's contradicted or detected stale. It defaults to 0.5 —
// deliberately neutral, because the ranking multiplier in EffectiveSalienceExpr
// is 1.0 at exactly 0.5, so existing rows rank identically until the feedback
// loop actually moves their confidence. `decision_feedback` is an append-only
// audit log of every confidence-changing event.
func applyV8(db *sql.DB) error {
	if err := addColumnIfMissing(db, "decisions", "confidence", "REAL NOT NULL DEFAULT 0.5"); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS decision_feedback (
		  id          INTEGER PRIMARY KEY AUTOINCREMENT,
		  decision_id INTEGER NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
		  kind        TEXT NOT NULL,   -- 'confirmed' | 'contradicted' | 'stale'
		  ts          INTEGER NOT NULL,
		  weight      REAL NOT NULL DEFAULT 1.0,
		  note        TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_feedback_decision ON decision_feedback(decision_id);
	`); err != nil {
		return err
	}
	return nil
}

func applyV7(db *sql.DB) error {
	if err := addColumnIfMissing(db, "turns", "distilled_at", "INTEGER"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_turns_undistilled
		  ON turns(ts) WHERE distilled_at IS NULL`); err != nil {
		return err
	}
	return nil
}

func applyV6(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS entities (
		  id            INTEGER PRIMARY KEY AUTOINCREMENT,
		  name          TEXT NOT NULL UNIQUE,
		  display       TEXT NOT NULL,
		  first_seen    INTEGER NOT NULL,
		  last_seen     INTEGER NOT NULL,
		  mention_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS entity_mentions (
		  entity_id   INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
		  source_kind TEXT NOT NULL CHECK (source_kind IN ('turn','decision')),
		  source_id   TEXT NOT NULL,
		  ts          INTEGER NOT NULL,
		  PRIMARY KEY (entity_id, source_kind, source_id)
		);
		CREATE INDEX IF NOT EXISTS idx_mentions_source ON entity_mentions(source_kind, source_id);
		CREATE INDEX IF NOT EXISTS idx_mentions_ts     ON entity_mentions(ts);
	`); err != nil {
		return err
	}
	return nil
}

func applyV5(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS decision_paraphrases (
		  id          INTEGER PRIMARY KEY AUTOINCREMENT,
		  decision_id INTEGER NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
		  text        TEXT NOT NULL,
		  embedding   BLOB,
		  ts          INTEGER,
		  UNIQUE(decision_id, text)
		);
		CREATE INDEX IF NOT EXISTS idx_paraphrases_decision ON decision_paraphrases(decision_id);
		CREATE INDEX IF NOT EXISTS idx_paraphrases_missing_emb
		  ON decision_paraphrases(decision_id) WHERE embedding IS NULL;
	`); err != nil {
		return err
	}
	return nil
}

func applyV4(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS pins (
		  id          INTEGER PRIMARY KEY AUTOINCREMENT,
		  session_id  TEXT,
		  project_dir TEXT,
		  ts          INTEGER NOT NULL,
		  text        TEXT NOT NULL,
		  cleared_at  INTEGER
		);
		CREATE INDEX IF NOT EXISTS idx_pins_session ON pins(session_id) WHERE cleared_at IS NULL;
		CREATE INDEX IF NOT EXISTS idx_pins_project ON pins(project_dir) WHERE cleared_at IS NULL;
	`); err != nil {
		return err
	}
	return nil
}

func applyV3(db *sql.DB) error {
	if err := addColumnIfMissing(db, "sessions", "summary_consolidated_at", "INTEGER"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "sessions", "summary_source", "TEXT"); err != nil {
		return err
	}
	return nil
}

func applyV2(db *sql.DB) error {
	if err := addColumnIfMissing(db, "decisions", "embedding", "BLOB"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "turns", "embedding", "BLOB"); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tool_events (
		  id          INTEGER PRIMARY KEY AUTOINCREMENT,
		  ts          INTEGER NOT NULL,
		  cwd         TEXT,
		  tool        TEXT NOT NULL,
		  file_path   TEXT,
		  before_hash TEXT,
		  after_hash  TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_tool_events_path_ts ON tool_events(file_path, ts);
	`); err != nil {
		return err
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, typ string) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, typ))
	return err
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply base schema: %w", err)
	}
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	current := int(v.Int64)
	if !v.Valid {
		current = 1 // base schema implicit version
	}
	if current > currentSchemaVersion {
		return fmt.Errorf("db schema_version=%d > supported %d (downgrade?)", current, currentSchemaVersion)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := m.apply(s.db); err != nil {
			return fmt.Errorf("migration v%d: %w", m.version, err)
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO schema_version VALUES (?)`, m.version); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
	}
	return nil
}

func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &Tx{tx: sqlTx}
	if err := fn(tx); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

type Tx struct{ tx *sql.Tx }

// QueryRow exposes the underlying transaction for callers that need ad-hoc reads
// inside a Tx (e.g. INSERT ... RETURNING).
func (t *Tx) QueryRow(q string, args ...any) *sql.Row { return t.tx.QueryRow(q, args...) }

// Exec exposes the underlying transaction for ad-hoc writes (e.g. forget).
func (t *Tx) Exec(q string, args ...any) (sql.Result, error) { return t.tx.Exec(q, args...) }

// EffectiveSalienceExpr is a SQL expression for the effective salience of a
// decision row, accounting for time decay, reuse, and trust. Composes as:
//
//	(base_salience * exp(-age_days / half_life_days) + use_boost * ln(1 + use_count))
//	  * trust_multiplier
//
// half_life_days = 30  (a 30-day-old memory counts at ~37% of its base score)
// use_boost      = 0.1 (each doubling of uses adds ~0.07 to salience)
// trust_mult     = 0.6 + 0.8 * confidence, clamped by confidence ∈ [0,1] to
//
//	[0.6, 1.4]. At the default confidence 0.5 this is exactly
//	1.0, so un-reinforced rows rank identically to the pre-v8
//	behaviour; corroborated facts (confidence→1) get up to a
//	40% boost, contradicted/stale ones (confidence→0) up to a
//	40% penalty — but never zeroed, so nothing silently vanishes.
//
// Use this expression directly in ORDER BY clauses; the row's `id`, `salience`,
// `ts`, `use_count`, and `confidence` columns must be in scope. Falls back to
// current time if `ts` is NULL and to neutral 0.5 if `confidence` is NULL.
const EffectiveSalienceExpr = `(
	(
		salience
		* EXP(-(unixepoch() - COALESCE(ts, unixepoch())) / 86400.0 / 30.0)
		+ 0.1 * ln(1 + COALESCE(use_count, 0))
	)
	* (0.6 + 0.8 * COALESCE(confidence, 0.5))
)`

// BumpUseCount increments use_count and updates last_used_at for the given decision IDs.
// Best-effort: errors are swallowed since salience tracking should never fail a query.
func (s *Store) BumpUseCount(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().Unix())
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := `UPDATE decisions
	         SET use_count = use_count + 1,
	             last_used_at = ?
	       WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	_, _ = s.db.ExecContext(ctx, q, args...)
}

// Confidence step constants for the trust feedback loop.
const (
	// confirmStep moves confidence toward 1.0 on each corroboration:
	// new = c + (1-c)*step. At 0.34 a neutral 0.5 fact reaches ~0.86 after
	// three confirmations and asymptotes at 1.0 — fast enough to matter,
	// slow enough that a single coincidental near-duplicate doesn't max it.
	confirmStep = 0.34
	// weakenFactor multiplies confidence on contradiction/staleness. A
	// contradicted 0.5 fact drops to 0.25; it can recover via later
	// confirmations rather than being deleted outright.
	weakenFactor = 0.5
)

// RecordFeedback appends an audit row and adjusts the decision's confidence
// in one transaction. `kind` is 'confirmed' (raise), 'contradicted' or
// 'stale' (lower). A confirm also counts as a use (bumps use_count). Returns
// the new confidence. Best-effort callers may ignore the error — trust
// tracking must never fail the primary write.
func (s *Store) RecordFeedback(ctx context.Context, id int64, kind, note string) (float64, error) {
	var newConf float64
	err := s.Tx(ctx, func(tx *Tx) error {
		now := time.Now().Unix()
		switch kind {
		case "confirmed":
			if _, err := tx.tx.Exec(
				`UPDATE decisions
				    SET confidence = MIN(1.0, confidence + (1.0 - confidence) * ?),
				        use_count  = use_count + 1,
				        last_used_at = ?
				  WHERE id = ?`, confirmStep, now, id); err != nil {
				return err
			}
		case "contradicted", "stale":
			if _, err := tx.tx.Exec(
				`UPDATE decisions SET confidence = MAX(0.0, confidence * ?) WHERE id = ?`,
				weakenFactor, id); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown feedback kind %q", kind)
		}
		if _, err := tx.tx.Exec(
			`INSERT INTO decision_feedback(decision_id, kind, ts, weight, note)
			 VALUES (?, ?, ?, 1.0, NULLIF(?, ''))`,
			id, kind, now, note); err != nil {
			return err
		}
		return tx.tx.QueryRow(`SELECT confidence FROM decisions WHERE id = ?`, id).Scan(&newConf)
	})
	return newConf, err
}

// MergeDuplicate folds loser into winner in one transaction: the winner
// absorbs the loser's use_count, gets a confidence bump (the duplicate is
// corroboration), supersedes the loser, keeps the loser's phrasing as a
// paraphrase (so the memory stays findable from that wording), and logs the
// merge to decision_feedback. Used by the maintenance dedup pass.
func (s *Store) MergeDuplicate(ctx context.Context, winnerID, loserID int64, loserText string, loserEmb []byte) error {
	if winnerID == loserID {
		return nil
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.Exec(
			`UPDATE decisions
			    SET use_count  = use_count + COALESCE((SELECT use_count FROM decisions WHERE id = ?), 0),
			        confidence = MIN(1.0, confidence + (1.0 - confidence) * ?)
			  WHERE id = ?`, loserID, confirmStep, winnerID); err != nil {
			return err
		}
		if err := tx.SupersedeDecisions(winnerID, []int64{loserID}); err != nil {
			return err
		}
		if _, err := tx.InsertParaphrase(Paraphrase{DecisionID: winnerID, Text: loserText, Embedding: loserEmb}); err != nil {
			return err
		}
		_, err := tx.tx.Exec(
			`INSERT INTO decision_feedback(decision_id, kind, ts, weight, note)
			 VALUES (?, 'confirmed', ?, 1.0, ?)`,
			winnerID, time.Now().Unix(), fmt.Sprintf("merged duplicate #%d", loserID))
		return err
	})
}

// DeleteDecisions hard-deletes the given decision ids (FK cascades drop their
// paraphrases + feedback rows). Used by the maintenance decay pass to age out
// stale, low-salience, auto-extracted rows. Returns the number deleted.
func (s *Store) DeleteDecisions(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	var n int64
	err := s.Tx(ctx, func(tx *Tx) error {
		res, err := tx.tx.Exec(`DELETE FROM decisions WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// maintainSourceKind is a synthetic `sources` row used only to persist the
// last-maintenance timestamp in ingest_state — no real ingestion happens
// against it. Lets `recall watch` gate the daily maintenance pass without a
// new schema object.
const maintainSourceKind = "maintenance"

// LastMaintainAt returns when the maintenance pass last ran (zero time if
// never). The timestamp lives in ingest_state.last_offset for the synthetic
// maintenance source.
func (s *Store) LastMaintainAt(ctx context.Context) (time.Time, error) {
	var unix int64
	err := s.db.QueryRowContext(ctx, `
		SELECT i.last_offset
		  FROM ingest_state i
		  JOIN sources s ON s.id = i.source_id
		 WHERE s.kind = ? AND i.key = 'last_run'`, maintainSourceKind).Scan(&unix)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0), nil
}

// SetMaintainAt records when the maintenance pass last ran.
func (s *Store) SetMaintainAt(ctx context.Context, t time.Time) error {
	return s.Tx(ctx, func(tx *Tx) error {
		id, err := tx.UpsertSource(maintainSourceKind, "/maintenance")
		if err != nil {
			return err
		}
		return tx.SetIngestState(id, "last_run", IngestState{LastOffset: t.Unix()})
	})
}

// EntityCard is a rolled-up, LLM-distilled summary of everything known about
// one @-entity, with the entity's display name attached for rendering.
type EntityCard struct {
	EntityID    int64
	Name        string // normalized
	Display     string // original casing
	Summary     string
	SourceCount int
	RefreshedAt int64
}

// UpsertEntityCard inserts or replaces the wiki card for an entity.
func (s *Store) UpsertEntityCard(ctx context.Context, entityID int64, summary string, embedding []byte, sourceCount int) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.tx.Exec(
			`INSERT INTO entity_cards(entity_id, summary, embedding, source_count, refreshed_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(entity_id) DO UPDATE SET
			   summary      = excluded.summary,
			   embedding    = excluded.embedding,
			   source_count = excluded.source_count,
			   refreshed_at = excluded.refreshed_at`,
			entityID, summary, embedding, sourceCount, time.Now().Unix())
		return err
	})
}

// GetEntityCard returns the wiki card for an entity by (case-insensitive)
// name. found=false when the entity is unknown or has no card yet.
func (s *Store) GetEntityCard(ctx context.Context, name string) (EntityCard, bool, error) {
	var c EntityCard
	err := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.name, e.display, c.summary, c.source_count, c.refreshed_at
		  FROM entity_cards c
		  JOIN entities e ON e.id = c.entity_id
		 WHERE e.name = ?`, strings.ToLower(strings.TrimSpace(name))).
		Scan(&c.EntityID, &c.Name, &c.Display, &c.Summary, &c.SourceCount, &c.RefreshedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EntityCard{}, false, nil
	}
	if err != nil {
		return EntityCard{}, false, err
	}
	return c, true, nil
}

// ListEntityCards returns existing cards, most recently refreshed first.
func (s *Store) ListEntityCards(ctx context.Context, limit int) ([]EntityCard, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.name, e.display, c.summary, c.source_count, c.refreshed_at
		  FROM entity_cards c
		  JOIN entities e ON e.id = c.entity_id
		 ORDER BY c.refreshed_at DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityCard
	for rows.Next() {
		var c EntityCard
		if err := rows.Scan(&c.EntityID, &c.Name, &c.Display, &c.Summary, &c.SourceCount, &c.RefreshedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (t *Tx) UpsertSource(kind, path string) (int64, error) {
	if _, err := t.tx.Exec(
		`INSERT INTO sources(kind, path) VALUES(?, ?)
		 ON CONFLICT(kind) DO UPDATE SET path=excluded.path`,
		kind, path,
	); err != nil {
		return 0, err
	}
	var id int64
	if err := t.tx.QueryRow(`SELECT id FROM sources WHERE kind=?`, kind).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (t *Tx) MarkSourceIngested(id int64) error {
	_, err := t.tx.Exec(`UPDATE sources SET last_ingested_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

type Session struct {
	ID            string
	SourceID      int64
	ProjectDir    string
	GitBranch     string
	SourceVersion string
	StartedAt     int64
	EndedAt       int64
	Summary       string
}

// UpsertSession inserts or merges a session row. Strings overwrite only when non-empty;
// started_at/ended_at update to the min/max across runs.
func (t *Tx) UpsertSession(s Session) error {
	_, err := t.tx.Exec(
		`INSERT INTO sessions(id, source_id, project_dir, git_branch, source_version, started_at, ended_at, summary)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		    project_dir    = COALESCE(NULLIF(excluded.project_dir, ''),    sessions.project_dir),
		    git_branch     = COALESCE(NULLIF(excluded.git_branch, ''),     sessions.git_branch),
		    source_version = COALESCE(NULLIF(excluded.source_version, ''), sessions.source_version),
		    started_at = CASE
		        WHEN sessions.started_at IS NULL THEN excluded.started_at
		        WHEN excluded.started_at IS NULL THEN sessions.started_at
		        ELSE MIN(sessions.started_at, excluded.started_at)
		    END,
		    ended_at = CASE
		        WHEN sessions.ended_at IS NULL THEN excluded.ended_at
		        WHEN excluded.ended_at IS NULL THEN sessions.ended_at
		        ELSE MAX(sessions.ended_at, excluded.ended_at)
		    END,
		    summary = COALESCE(NULLIF(excluded.summary, ''), sessions.summary)
		`,
		s.ID, s.SourceID,
		nullStr(s.ProjectDir), nullStr(s.GitBranch), nullStr(s.SourceVersion),
		nullInt(s.StartedAt), nullInt(s.EndedAt), nullStr(s.Summary),
	)
	return err
}

type Turn struct {
	UUID       string
	SessionID  string
	ParentUUID string
	Idx        int
	Role       string
	Ts         int64
	Text       string
	HasToolUse bool
}

// InsertTurn does INSERT OR IGNORE keyed on uuid. Returns true if a new row was written.
func (t *Tx) InsertTurn(tn Turn) (bool, error) {
	res, err := t.tx.Exec(
		`INSERT OR IGNORE INTO turns(uuid, session_id, parent_uuid, idx, role, ts, text, has_tool_use)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		tn.UUID, tn.SessionID, nullStr(tn.ParentUUID), tn.Idx, tn.Role,
		nullInt(tn.Ts), tn.Text, boolInt(tn.HasToolUse),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type File struct {
	SessionID  string
	TurnUUID   string
	ProjectDir string
	Path       string
	Op         string
	Ts         int64
}

// UpsertEdge increments the weight of an edge by 1, or inserts at weight 1
// if absent. Used by the walker to populate the co-occurrence graph during
// ingest (file↔session, file↔file pairs in the same session).
func (t *Tx) UpsertEdge(srcKind, srcID, dstKind, dstID string) error {
	_, err := t.tx.Exec(
		`INSERT INTO edges(src_kind, src_id, dst_kind, dst_id, weight)
		 VALUES (?, ?, ?, ?, 1.0)
		 ON CONFLICT(src_kind, src_id, dst_kind, dst_id)
		   DO UPDATE SET weight = weight + 1.0`,
		srcKind, srcID, dstKind, dstID,
	)
	return err
}

// InsertFile does INSERT OR IGNORE; UNIQUE(turn_uuid,path,op) handles dedup.
func (t *Tx) InsertFile(f File) (bool, error) {
	res, err := t.tx.Exec(
		`INSERT OR IGNORE INTO files(session_id, turn_uuid, project_dir, path, op, ts)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		f.SessionID, nullStr(f.TurnUUID), nullStr(f.ProjectDir), f.Path, f.Op, nullInt(f.Ts),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type Decision struct {
	ProjectDir string
	SessionID  string
	Ts         int64
	Kind       string
	Text       string
	Source     string
	Salience   float64
}

// InsertDecision inserts a decision and returns its id. Used by the
// reconsolidation path which needs the new id immediately to write
// `superseded_by` on contradicted predecessors.
func (t *Tx) InsertDecision(d Decision) (int64, error) {
	if d.Source == "" {
		d.Source = "cli"
	}
	if d.Salience == 0 {
		d.Salience = 1.0
	}
	var id int64
	err := t.tx.QueryRow(
		`INSERT INTO decisions(project_dir, session_id, ts, kind, text, source, salience)
		 VALUES (NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)
		 RETURNING id`,
		d.ProjectDir, d.SessionID, nullInt(d.Ts), d.Kind, d.Text, d.Source, d.Salience,
	).Scan(&id)
	return id, err
}

// SetEmbedding writes a vector blob onto an existing decision id.
func (t *Tx) SetEmbedding(decisionID int64, embedding []byte) error {
	_, err := t.tx.Exec(`UPDATE decisions SET embedding = ? WHERE id = ?`, embedding, decisionID)
	return err
}

// SetTurnEmbedding writes a vector blob onto an existing turn UUID.
func (t *Tx) SetTurnEmbedding(uuid string, embedding []byte) error {
	_, err := t.tx.Exec(`UPDATE turns SET embedding = ? WHERE uuid = ?`, embedding, uuid)
	return err
}

// Pin is a "remember this for the rest of the session" marker. Pins always
// surface at the top of the inject block regardless of the project's
// salience ordering — they're the user's explicit pins.
type Pin struct {
	ID         int64
	SessionID  string
	ProjectDir string
	Ts         int64
	Text       string
}

// CreatePin inserts a new pin. session_id may be empty (project-scoped pin).
func (t *Tx) CreatePin(p Pin) (int64, error) {
	if p.Ts == 0 {
		p.Ts = time.Now().Unix()
	}
	var id int64
	err := t.tx.QueryRow(
		`INSERT INTO pins(session_id, project_dir, ts, text)
		 VALUES (NULLIF(?, ''), NULLIF(?, ''), ?, ?)
		 RETURNING id`,
		p.SessionID, p.ProjectDir, p.Ts, p.Text,
	).Scan(&id)
	return id, err
}

// ClearPin soft-deletes by setting cleared_at. Listing skips cleared rows.
func (t *Tx) ClearPin(id int64) error {
	_, err := t.tx.Exec(
		`UPDATE pins SET cleared_at = ? WHERE id = ? AND cleared_at IS NULL`,
		time.Now().Unix(), id,
	)
	return err
}

// ActivePins returns pins for the given session (if non-empty) plus pins
// scoped to the project (no session). Project filter uses ancestor matching
// so /Users/z pins surface for /Users/z/Documents/recall.
func (s *Store) ActivePins(ctx context.Context, sessionID, projectDir string, limit int) ([]Pin, error) {
	if limit <= 0 {
		limit = 20
	}
	var (
		where = "cleared_at IS NULL"
		args  []any
	)
	switch {
	case sessionID != "" && projectDir != "":
		where += ` AND (session_id = ?
		             OR (session_id IS NULL AND (project_dir = ? OR ? LIKE (project_dir || '/%') OR project_dir IS NULL)))`
		args = append(args, sessionID, projectDir, projectDir)
	case sessionID != "":
		where += ` AND session_id = ?`
		args = append(args, sessionID)
	case projectDir != "":
		where += ` AND session_id IS NULL AND (project_dir = ? OR ? LIKE (project_dir || '/%') OR project_dir IS NULL)`
		args = append(args, projectDir, projectDir)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(session_id, ''), COALESCE(project_dir, ''), ts, text
		   FROM pins
		  WHERE `+where+
			` ORDER BY ts DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pin
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.ID, &p.SessionID, &p.ProjectDir, &p.Ts, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Paraphrase is one alternate phrasing of a decision, used to make the
// memory findable from differently-worded queries. Each row carries its
// own embedding; semantic search compares queries against canonical
// embeddings AND paraphrase embeddings, taking the max cosine per
// decision to compute the final score.
type Paraphrase struct {
	ID         int64
	DecisionID int64
	Text       string
	Embedding  []byte
	Ts         int64
}

// InsertParaphrase upserts a paraphrase for a decision. Idempotent on
// (decision_id, text) — re-running keeps the row but updates ts and
// optionally embedding.
func (t *Tx) InsertParaphrase(p Paraphrase) (int64, error) {
	if p.Ts == 0 {
		p.Ts = time.Now().Unix()
	}
	res, err := t.tx.Exec(
		`INSERT INTO decision_paraphrases(decision_id, text, embedding, ts)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(decision_id, text) DO UPDATE SET
		   ts = excluded.ts,
		   embedding = COALESCE(excluded.embedding, decision_paraphrases.embedding)`,
		p.DecisionID, p.Text, p.Embedding, p.Ts,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// SetParaphraseEmbedding writes the embedding blob for a paraphrase.
func (t *Tx) SetParaphraseEmbedding(id int64, embedding []byte) error {
	_, err := t.tx.Exec(`UPDATE decision_paraphrases SET embedding = ? WHERE id = ?`, embedding, id)
	return err
}

// SupersedeDecisions marks oldIDs as superseded_by newID. Idempotent.
func (t *Tx) SupersedeDecisions(newID int64, oldIDs []int64) error {
	for _, oid := range oldIDs {
		if oid == newID {
			continue
		}
		if _, err := t.tx.Exec(
			`UPDATE decisions SET superseded_by = ? WHERE id = ? AND superseded_by IS NULL`,
			newID, oid,
		); err != nil {
			return err
		}
	}
	return nil
}

// InsertDecisionIfNew inserts when no active decision with the same (text, project, source-bucket)
// exists. The source-bucket groups pattern/tool/cli (human-or-agent-asserted) so we don't get
// duplicate rows from the regex extractor seeing the same thing across sessions. Returns
// (id, inserted) where id is 0 when inserted is false.
func (t *Tx) InsertDecisionIfNew(d Decision) (int64, bool, error) {
	if d.Source == "" {
		d.Source = "pattern"
	}
	if d.Salience == 0 {
		d.Salience = 1.0
	}
	res, err := t.tx.Exec(
		`INSERT INTO decisions(project_dir, session_id, ts, kind, text, source, salience)
		 SELECT NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?
		  WHERE NOT EXISTS (
		    SELECT 1 FROM decisions
		     WHERE LOWER(text) = LOWER(?)
		       AND COALESCE(project_dir, '') = COALESCE(NULLIF(?, ''), '')
		       AND source IN ('pattern', 'tool', 'cli', 'user-memory-md')
		       AND superseded_by IS NULL
		  )`,
		d.ProjectDir, d.SessionID, nullInt(d.Ts), d.Kind, d.Text, d.Source, d.Salience,
		d.Text, d.ProjectDir,
	)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

type IngestState struct {
	LastOffset int64
	LastLine   int
	LastUUID   string
}

func (t *Tx) GetIngestState(sourceID int64, key string) (IngestState, error) {
	var st IngestState
	var lastUUID sql.NullString
	err := t.tx.QueryRow(
		`SELECT last_offset, last_line, last_uuid FROM ingest_state WHERE source_id=? AND key=?`,
		sourceID, key,
	).Scan(&st.LastOffset, &st.LastLine, &lastUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestState{}, nil
	}
	if err != nil {
		return IngestState{}, err
	}
	st.LastUUID = lastUUID.String
	return st, nil
}

func (t *Tx) SetIngestState(sourceID int64, key string, st IngestState) error {
	_, err := t.tx.Exec(
		`INSERT INTO ingest_state(source_id, key, last_offset, last_line, last_uuid, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source_id, key) DO UPDATE SET
		    last_offset = excluded.last_offset,
		    last_line   = excluded.last_line,
		    last_uuid   = excluded.last_uuid,
		    updated_at  = excluded.updated_at`,
		sourceID, key, st.LastOffset, st.LastLine, nullStr(st.LastUUID), time.Now().Unix(),
	)
	return err
}

type Stats struct {
	Sessions      int
	Turns         int
	Files         int
	Projects      int
	OldestSession time.Time
	NewestSession time.Time
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&st.Sessions); err != nil {
		return st, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&st.Turns); err != nil {
		return st, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&st.Files); err != nil {
		return st, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT project_dir) FROM sessions WHERE project_dir IS NOT NULL`).Scan(&st.Projects); err != nil {
		return st, err
	}
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(started_at), MAX(ended_at) FROM sessions`).Scan(&oldest, &newest); err != nil {
		return st, err
	}
	if oldest.Valid {
		st.OldestSession = time.Unix(oldest.Int64, 0)
	}
	if newest.Valid {
		st.NewestSession = time.Unix(newest.Int64, 0)
	}
	return st, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt(i int64) sql.NullInt64 {
	if i == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: i, Valid: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
