package continueide

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
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Session 1 with string content
	s1 := map[string]interface{}{
		"sessionId":          "sess-1",
		"title":              "Refactor handler",
		"workspaceDirectory": "/Users/dev/proj-a",
		"history": []map[string]interface{}{
			{"message": map[string]interface{}{"role": "user", "content": "fix the auth bug"}},
			{"message": map[string]interface{}{"role": "assistant", "content": "looking at handler.go"}},
		},
	}
	writeJSON(t, filepath.Join(root, "sess-1.json"), s1)

	// Session 2 with array-of-blocks content
	s2 := map[string]interface{}{
		"sessionId":          "sess-2",
		"title":              "Add metrics",
		"workspaceDirectory": "/Users/dev/proj-b",
		"history": []map[string]interface{}{
			{"message": map[string]interface{}{"role": "user", "content": []map[string]interface{}{
				{"type": "text", "text": "add prometheus metrics"},
			}}},
			{"message": map[string]interface{}{"role": "assistant", "content": "adding /metrics endpoint"}},
		},
	}
	writeJSON(t, filepath.Join(root, "sess-2.json"), s2)

	// sessions.json index — gives us a dateCreated to test the path
	idx := []map[string]interface{}{
		{"sessionId": "sess-1", "title": "Refactor handler", "dateCreated": "1700000000000", "workspaceDirectory": "/Users/dev/proj-a"},
		{"sessionId": "sess-2", "title": "Add metrics", "dateCreated": "1700001000000", "workspaceDirectory": "/Users/dev/proj-b"},
	}
	writeJSON(t, filepath.Join(root, "sessions.json"), idx)

	return root
}

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	raw, _ := json.Marshal(v)
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
	root := setupFixture(t)
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !stats.RootExists {
		t.Fatal("RootExists=false on fixture")
	}
	if stats.NewSessions != 2 {
		t.Errorf("new sessions=%d, want 2", stats.NewSessions)
	}
	if stats.NewTurns != 4 {
		t.Errorf("new turns=%d, want 4", stats.NewTurns)
	}

	// Session 1's started_at should come from sessions.json index.
	row := st.DB().QueryRow(`SELECT started_at, project_dir FROM sessions WHERE id='continue:sess-1'`)
	var started int64
	var projDir string
	if err := row.Scan(&started, &projDir); err != nil {
		t.Fatal(err)
	}
	if started != 1700000000000 {
		t.Errorf("started_at=%d, want 1700000000000 (from index)", started)
	}
	if projDir != "/Users/dev/proj-a" {
		t.Errorf("project_dir=%q", projDir)
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
		t.Errorf("second pass added new(%d,%d); want (0,0)", second.NewSessions, second.NewTurns)
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
