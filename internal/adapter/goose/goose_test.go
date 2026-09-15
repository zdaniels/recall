package goose

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/store"
)

func setupFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			working_dir TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content_json TEXT NOT NULL,
			created_timestamp INTEGER NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO sessions(id, name, working_dir, created_at, updated_at) VALUES
		  ('g-1', 'Refactor', '/code/projA', datetime('2026-05-01 10:00:00'), datetime('2026-05-01 10:30:00'));
		-- content_json variants: plain string, {text}-object, array-of-blocks
		INSERT INTO messages(session_id, role, content_json, created_timestamp) VALUES
		  ('g-1', 'user',      '"first turn"',                                  1714557600000),
		  ('g-1', 'assistant', '{"text":"thinking about it"}',                  1714557610000),
		  ('g-1', 'user',      '[{"text":"follow-up"},{"name":"read_file"}]',   1714557620000),
		  ('g-1', 'tool',      '"{}"',                                          1714557625000);
	`); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "recall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngestHappyPath(t *testing.T) {
	dbPath := setupFixtureDB(t)
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, dbPath)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !stats.DBExists {
		t.Fatal("DBExists=false")
	}
	if stats.NewSessions != 1 {
		t.Errorf("new sessions=%d, want 1", stats.NewSessions)
	}
	// All 4 messages have non-empty content after extractText; the
	// array-of-blocks turn should produce "follow-up\n[tool: read_file]"
	if stats.NewTurns != 4 {
		t.Errorf("new turns=%d, want 4", stats.NewTurns)
	}

	row := st.DB().QueryRow(`SELECT text FROM turns WHERE session_id='goose:g-1' AND idx=3`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "follow-up\n[tool: read_file]" {
		t.Errorf("turn 3 text=%q, want 'follow-up\\n[tool: read_file]'", text)
	}
}

func TestIngestIdempotent(t *testing.T) {
	dbPath := setupFixtureDB(t)
	st := openTempStore(t)
	_, _ = Ingest(context.Background(), st, dbPath)
	second, err := Ingest(context.Background(), st, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewSessions != 0 || second.NewTurns != 0 {
		t.Errorf("second pass added new(%d, %d); want (0, 0)", second.NewSessions, second.NewTurns)
	}
}

func TestIngestMissingDB(t *testing.T) {
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, filepath.Join(t.TempDir(), "nope.db"))
	if err != nil {
		t.Errorf("missing db should be silent, got %v", err)
	}
	if stats.DBExists {
		t.Error("DBExists should be false")
	}
}
