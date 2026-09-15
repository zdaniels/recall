package roocode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/store"
)

func setupFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// One task with two messages — Roo's ts field is on each api message
	// directly (no ui_messages.json needed).
	taskID := "1700000000000"
	taskDir := filepath.Join(root, taskID)
	os.MkdirAll(taskDir, 0o755)
	api := []map[string]interface{}{
		{"role": "user", "content": "look at auth.go", "ts": 1700000005000},
		{"role": "assistant", "content": []map[string]interface{}{
			{"type": "text", "text": "checking it"},
			{"type": "tool_use", "name": "read_file"},
			{"type": "reasoning", "text": "thinking about the issuer check"},
		}, "ts": 1700000020000},
	}
	raw, _ := json.Marshal(api)
	if err := os.WriteFile(filepath.Join(taskDir, "api_conversation_history.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
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
	root := setupFixture(t)
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !stats.RootExists {
		t.Fatal("RootExists=false")
	}
	if stats.NewSessions != 1 {
		t.Errorf("new sessions=%d, want 1", stats.NewSessions)
	}
	if stats.NewTurns != 2 {
		t.Errorf("new turns=%d, want 2", stats.NewTurns)
	}

	// Roo's per-message ts pulled through (no ui_messages dance needed)
	row := st.DB().QueryRow(`SELECT ts FROM turns WHERE session_id='roocode:1700000000000' AND idx=2`)
	var ts int64
	if err := row.Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != 1700000020000 {
		t.Errorf("idx=2 ts=%d, want 1700000020000", ts)
	}

	// Multi-block content should preserve text + tool_use + reasoning tags
	row = st.DB().QueryRow(`SELECT text FROM turns WHERE session_id='roocode:1700000000000' AND idx=2`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if want := "checking it\n[tool_use: read_file]\n[reasoning] thinking about the issuer check"; text != want {
		t.Errorf("turn 2 text=%q, want %q", text, want)
	}
}

func TestIngestIdempotent(t *testing.T) {
	root := setupFixture(t)
	st := openTempStore(t)
	_, _ = Ingest(context.Background(), st, root)
	second, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewSessions != 0 || second.NewTurns != 0 {
		t.Errorf("second pass added new(%d, %d); want (0, 0)", second.NewSessions, second.NewTurns)
	}
}

func TestIngestMissingRoot(t *testing.T) {
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("missing root should be silent, got %v", err)
	}
	if stats.RootExists {
		t.Error("RootExists should be false")
	}
}
