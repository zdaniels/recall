package zed

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/store"
)

func setupFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "threads.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE threads (
			id TEXT PRIMARY KEY,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			created_at TEXT
		);
	`); err != nil {
		t.Fatal(err)
	}

	thread := map[string]interface{}{
		"title": "Refactor login",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "look at the auth flow", "updated_at": "2026-05-01T10:00:00Z"},
			{"role": "assistant", "content": []map[string]interface{}{
				{"type": "text", "text": "inspecting login.go"},
				{"type": "tool_use", "name": "read_file"},
			}, "updated_at": "2026-05-01T10:00:30Z"},
		},
	}
	jb, _ := json.Marshal(thread)

	// Encode as zstd (the default data_type)
	enc, _ := zstd.NewWriter(nil)
	compressed := enc.EncodeAll(jb, nil)
	enc.Close()

	if _, err := db.Exec(`
		INSERT INTO threads(id, summary, updated_at, data_type, data, folder_paths, created_at)
		VALUES('zt-1', 'Refactor login', '2026-05-01T10:00:30Z', 'Zstd', ?, '/code/projA', '2026-05-01T10:00:00Z')
	`, compressed); err != nil {
		t.Fatal(err)
	}

	// Also insert an uncompressed JSON row to test the legacy path.
	if _, err := db.Exec(`
		INSERT INTO threads(id, summary, updated_at, data_type, data, folder_paths, created_at)
		VALUES('zt-2', 'Plain JSON', '2026-05-02T12:00:00Z', 'Json', ?, '/code/projB', '2026-05-02T12:00:00Z')
	`, jb); err != nil {
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
	if stats.Threads != 2 {
		t.Errorf("threads=%d, want 2", stats.Threads)
	}
	if stats.NewSessions != 2 {
		t.Errorf("new sessions=%d, want 2", stats.NewSessions)
	}
	// 2 messages per thread × 2 threads = 4
	if stats.NewTurns != 4 {
		t.Errorf("new turns=%d, want 4", stats.NewTurns)
	}

	// Per-message ts from updated_at on the dbMessage
	row := st.DB().QueryRow(`SELECT ts FROM turns WHERE session_id='zed:zt-1' AND idx=2`)
	var ts int64
	if err := row.Scan(&ts); err != nil {
		t.Fatal(err)
	}
	// 2026-05-01T10:00:30Z = 1777629630000 ms
	if ts != 1777629630000 {
		t.Errorf("idx=2 ts=%d, want 1777629630000", ts)
	}

	// Content extraction: array-of-blocks → text + tool tag
	row = st.DB().QueryRow(`SELECT text FROM turns WHERE session_id='zed:zt-1' AND idx=2`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "inspecting login.go\n[tool: read_file]" {
		t.Errorf("turn 2 text=%q", text)
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
