package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/store"
)

// fixture copies testdata/sample.jsonl into a temp Claude-Code-shaped tree
// so the discovery walker finds it.
func setupFixture(t *testing.T) (root, sessionPath string) {
	t.Helper()
	root = t.TempDir()
	projDir := filepath.Join(root, "-Users-test-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	sessionPath = filepath.Join(projDir, "test-session-1.jsonl")
	if err := os.WriteFile(sessionPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, sessionPath
}

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngest_GoldenFixture(t *testing.T) {
	root, _ := setupFixture(t)
	st := openTempStore(t)

	stats, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("errors=%d", stats.Errors)
	}
	// fixture has 8 lines: permission-mode, file-history-snapshot, user, assistant,
	// user, assistant, ai-title, future-unknown — 4 ingested as turns.
	if stats.NewTurns != 4 {
		t.Errorf("new turns=%d want 4", stats.NewTurns)
	}
	// file ops: Read README.md, Edit main.go, Write new.go = 3
	if stats.NewFiles != 3 {
		t.Errorf("new files=%d want 3", stats.NewFiles)
	}

	// session was upserted with the AI title, project_dir, branch, version
	row := st.DB().QueryRow(`SELECT project_dir, git_branch, source_version, summary FROM sessions WHERE id='test-session-1'`)
	var proj, branch, ver, summary string
	if err := row.Scan(&proj, &branch, &ver, &summary); err != nil {
		t.Fatal(err)
	}
	if proj != "/Users/test/proj" {
		t.Errorf("project_dir=%q", proj)
	}
	if branch != "main" {
		t.Errorf("git_branch=%q", branch)
	}
	if ver != "1.0.0" {
		t.Errorf("source_version=%q", ver)
	}
	if summary != "Test session for parser" {
		t.Errorf("summary=%q", summary)
	}

	// thinking should be excluded from indexed text
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM turn_fts WHERE turn_fts MATCH 'hidden'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("FTS leaked thinking text (matches=%d)", n)
	}

	// real text is indexed
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM turn_fts WHERE turn_fts MATCH 'README'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("MATCH 'README' = %d want 1", n)
	}
}

func TestIngest_Idempotent(t *testing.T) {
	root, _ := setupFixture(t)
	st := openTempStore(t)
	ctx := context.Background()

	first, err := Ingest(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ingest(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}

	if second.NewTurns != 0 || second.NewFiles != 0 {
		t.Fatalf("second run added rows: turns=%d files=%d (want 0,0)", second.NewTurns, second.NewFiles)
	}
	if second.LinesProcessed != 0 {
		t.Errorf("second run reprocessed lines: %d (incremental ingest broken)", second.LinesProcessed)
	}
	// totals unchanged
	s, _ := st.Stats()
	if s.Turns != first.NewTurns {
		t.Fatalf("turn count drift after idempotent run: %d != %d", s.Turns, first.NewTurns)
	}
}

func TestIngest_Incremental(t *testing.T) {
	root, sessionPath := setupFixture(t)
	st := openTempStore(t)
	ctx := context.Background()

	if _, err := Ingest(ctx, st, root); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Stats()

	// append a new user turn
	appendLine := []byte(`{"type":"user","uuid":"u-99","sessionId":"test-session-1","cwd":"/Users/test/proj","timestamp":"2026-04-30T05:01:00Z","message":{"role":"user","content":"appended later"}}` + "\n")
	f, err := os.OpenFile(sessionPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(appendLine); err != nil {
		t.Fatal(err)
	}
	f.Close()

	stats, err := Ingest(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LinesProcessed != 1 {
		t.Errorf("incremental processed=%d want 1", stats.LinesProcessed)
	}
	if stats.NewTurns != 1 {
		t.Errorf("new turns=%d want 1", stats.NewTurns)
	}
	after, _ := st.Stats()
	if after.Turns != before.Turns+1 {
		t.Errorf("total turns: %d → %d (want +1)", before.Turns, after.Turns)
	}
}
