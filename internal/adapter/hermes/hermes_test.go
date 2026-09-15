package hermes

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/store"
)

// setupFixtureDB builds a synthetic Hermes state.db with the same
// sessions+messages schema the upstream uses. Returns the path to the
// created file so the adapter under test gets the right DSN.
func setupFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			model TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			title TEXT
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			timestamp REAL NOT NULL,
			token_count INTEGER
		);
	`); err != nil {
		t.Fatal(err)
	}
	// Two sessions, with messages spanning user/assistant/tool roles.
	if _, err := db.Exec(`
		INSERT INTO sessions(id, source, model, started_at, ended_at, title) VALUES
		  ('sess-a', 'cli', 'claude-sonnet-4.5', 1700000000.0, 1700000060.0, 'Refactor handler'),
		  ('sess-b', 'cli', 'gpt-4o',            1700001000.0, 1700001120.0, 'Debug auth flow');
		INSERT INTO messages(session_id, role, content, timestamp, token_count) VALUES
		  ('sess-a', 'user',      'rewrite the http handler', 1700000010.0, 5),
		  ('sess-a', 'assistant', 'Here is the rewrite...',   1700000020.0, 42),
		  ('sess-a', 'tool',      '{"file":"handler.go"}',    1700000025.0, 0),
		  ('sess-a', 'user',      '',                          1700000030.0, 0),
		  ('sess-b', 'user',      'why is login failing',     1700001010.0, 5),
		  ('sess-b', 'assistant', 'CSRF token missing',       1700001020.0, 10);
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
		t.Fatal("DBExists=false on a real fixture file")
	}
	if stats.Sessions != 2 {
		t.Errorf("sessions=%d, want 2", stats.Sessions)
	}
	if stats.NewSessions != 2 {
		t.Errorf("new sessions=%d, want 2", stats.NewSessions)
	}
	// 6 messages; 1 is empty content and gets dropped — 5 turns.
	if stats.NewTurns != 5 {
		t.Errorf("new turns=%d, want 5", stats.NewTurns)
	}

	// Session summary should carry the model tag appended in parens
	row := st.DB().QueryRow(`SELECT summary FROM sessions WHERE id='hermes:sess-a'`)
	var summary string
	if err := row.Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "Refactor handler (claude-sonnet-4.5)" {
		t.Errorf("summary=%q", summary)
	}

	// One of the user turns content should be searchable in turns table
	row = st.DB().QueryRow(`SELECT text FROM turns WHERE session_id='hermes:sess-a' AND role='user' LIMIT 1`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "rewrite the http handler" {
		t.Errorf("user text=%q", text)
	}
}

func TestIngestIdempotent(t *testing.T) {
	dbPath := setupFixtureDB(t)
	st := openTempStore(t)

	first, err := Ingest(context.Background(), st, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ingest(context.Background(), st, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewSessions != 0 {
		t.Errorf("second pass added %d sessions; want 0 (idempotent)", second.NewSessions)
	}
	if second.NewTurns != 0 {
		t.Errorf("second pass added %d turns; want 0", second.NewTurns)
	}
	// Total turn count in store is unchanged between runs.
	var count int
	st.DB().QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&count)
	if count != first.NewTurns {
		t.Errorf("turn count drifted: store=%d, first pass=%d", count, first.NewTurns)
	}
}

func TestIngestMissingDB(t *testing.T) {
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, filepath.Join(t.TempDir(), "does-not-exist.db"))
	if err != nil {
		t.Errorf("expected nil error for missing DB, got %v", err)
	}
	if stats.DBExists {
		t.Error("DBExists should be false")
	}
	if stats.Sessions != 0 {
		t.Errorf("sessions=%d, want 0", stats.Sessions)
	}
}
