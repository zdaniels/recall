package cline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/store"
)

// setupFixtureRoot builds a synthetic Cline globalStorage layout —
// one root dir containing two task directories, each with
// api_conversation_history.json (and one with ui_messages.json so we
// also test per-turn timestamp extraction).
func setupFixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Task 1: timestamps 1700000000000 (start) → 1700000020000 (end)
	taskID1 := "1700000000000"
	taskDir1 := filepath.Join(root, taskID1)
	os.MkdirAll(taskDir1, 0o755)
	// Three messages: user (string content), assistant (string), user (content-block array).
	api1 := []map[string]interface{}{
		{"role": "user", "content": "look at the auth bug"},
		{"role": "assistant", "content": "checking handler.go"},
		{"role": "user", "content": []map[string]interface{}{
			{"type": "text", "text": "also check the JWT issuer"},
			{"type": "tool_use", "name": "read_file"},
		}},
	}
	writeJSON(t, filepath.Join(taskDir1, "api_conversation_history.json"), api1)
	// UI messages with conversationHistoryIndex for turns 0 and 1
	ui1 := []map[string]interface{}{
		{"ts": 1700000005000, "type": "say", "say": "text", "conversationHistoryIndex": 0},
		{"ts": 1700000020000, "type": "say", "say": "text", "conversationHistoryIndex": 1},
		// duplicate ts for index 1 (later) — adapter should keep the EARLIEST
		{"ts": 1700000025000, "type": "say", "say": "text", "conversationHistoryIndex": 1},
		// no conversationHistoryIndex on this one — ignored
		{"ts": 1700000030000, "type": "say"},
	}
	writeJSON(t, filepath.Join(taskDir1, "ui_messages.json"), ui1)

	// Task 2: only api_conversation_history.json (no UI log) — should
	// fall back to taskId as the ts for every turn.
	taskID2 := "1700001000000"
	taskDir2 := filepath.Join(root, taskID2)
	os.MkdirAll(taskDir2, 0o755)
	api2 := []map[string]interface{}{
		{"role": "user", "content": "different question"},
		{"role": "assistant", "content": "different answer"},
	}
	writeJSON(t, filepath.Join(taskDir2, "api_conversation_history.json"), api2)

	// Task 3: missing api_conversation_history.json — should be skipped silently.
	os.MkdirAll(filepath.Join(root, "1700002000000"), 0o755)

	return root
}

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
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
	root := setupFixtureRoot(t)
	st := openTempStore(t)

	stats, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !stats.RootExists {
		t.Fatal("RootExists=false on real fixture")
	}
	// 3 dirs created, but the 3rd has no api_conversation_history → 2 tasks ingested
	if stats.Tasks != 2 {
		t.Errorf("tasks=%d, want 2", stats.Tasks)
	}
	if stats.NewSessions != 2 {
		t.Errorf("new sessions=%d, want 2", stats.NewSessions)
	}
	// 3 turns task1 + 2 turns task2 = 5
	if stats.NewTurns != 5 {
		t.Errorf("new turns=%d, want 5", stats.NewTurns)
	}

	// Task 1 should have the earliest UI timestamp for index 1, not the duplicate.
	row := st.DB().QueryRow(`SELECT ts FROM turns WHERE session_id='cline:1700000000000' AND idx=2`)
	var ts int64
	if err := row.Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != 1700000020000 {
		t.Errorf("idx=2 ts=%d, want 1700000020000 (earliest of the two ui entries)", ts)
	}

	// Task 2 (no ui_messages.json) should fall back to the taskId as ts
	row = st.DB().QueryRow(`SELECT ts FROM turns WHERE session_id='cline:1700001000000' AND idx=1`)
	if err := row.Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != 1700001000000 {
		t.Errorf("fallback ts=%d, want 1700001000000 (taskId)", ts)
	}

	// Content-block array extraction: the third turn of task 1 should
	// have BOTH the text and a synthesized [tool_use: read_file] tag.
	row = st.DB().QueryRow(`SELECT text FROM turns WHERE session_id='cline:1700000000000' AND idx=3`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !contains(text, "JWT issuer") || !contains(text, "[tool_use: read_file]") {
		t.Errorf("expected text+tool_use in turn 3, got %q", text)
	}
}

func TestIngestIdempotent(t *testing.T) {
	root := setupFixtureRoot(t)
	st := openTempStore(t)

	_, _ = Ingest(context.Background(), st, root)
	second, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewSessions != 0 || second.NewTurns != 0 {
		t.Errorf("second pass added new(%d,%d); want (0,0)", second.NewSessions, second.NewTurns)
	}
}

func TestIngestMissingRoot(t *testing.T) {
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("missing root should be silent no-op, got %v", err)
	}
	if stats.RootExists {
		t.Error("RootExists should be false")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
