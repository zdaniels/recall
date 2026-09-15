package aider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/store"
)

// setupFixtureRoot makes a temp directory tree with two "projects",
// each containing a synthetic .aider.chat.history.md.
// Project A has two sessions (newer + older); Project B has one.
func setupFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	projA := filepath.Join(root, "code", "projectA")
	projB := filepath.Join(root, "work", "projectB")
	skipDir := filepath.Join(root, "code", "projectA", "node_modules", ".aider.chat.history.md") // shouldn't be walked
	for _, d := range []string{projA, projB, filepath.Dir(skipDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Project A history — two sessions
	a := `
# aider chat started at 2026-05-01 10:00:00

#### refactor the http handler
#### use the new error types

Sure, I'll rewrite handler.go to use the new error types.

The changes are: ...

#### look good?

Yes — tests pass.

# aider chat started at 2026-05-02 14:30:00

#### add the prometheus metrics

I'll add a /metrics endpoint to handler.go.
`
	if err := os.WriteFile(filepath.Join(projA, HistoryFile), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project B history — one session
	b := `# aider chat started at 2026-05-03 09:00:00

#### debug the auth flow

CSRF token check is failing.
`
	if err := os.WriteFile(filepath.Join(projB, HistoryFile), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}

	// File that should NOT be picked up — node_modules is on the skip list.
	if err := os.WriteFile(skipDir, []byte("# aider chat started at 2099-01-01 00:00:00\n#### ignore me\n"), 0o644); err != nil {
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
	root := setupFixtureRoot(t)
	st := openTempStore(t)

	stats, err := Ingest(context.Background(), st, root)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !stats.RootExists {
		t.Fatal("RootExists=false on fixture")
	}
	// 2 .aider.chat.history.md files found (node_modules one skipped)
	if stats.Files != 2 {
		t.Errorf("files=%d, want 2", stats.Files)
	}
	// 2 sessions in A + 1 in B = 3
	if stats.NewSessions != 3 {
		t.Errorf("new sessions=%d, want 3", stats.NewSessions)
	}
	// Project A session 1: the two leading '####' lines coalesce into
	//   one user turn (continuation lines), then assistant, then a
	//   third '####' user turn, then assistant — 4 total.
	// Project A session 2: 1 user + 1 assistant = 2
	// Project B:          1 user + 1 assistant = 2
	// Total = 8
	if stats.NewTurns != 8 {
		t.Errorf("new turns=%d, want 8", stats.NewTurns)
	}

	// The first user block of project-A's first session merges three
	// '####' lines into one turn (user input + continuation lines).
	row := st.DB().QueryRow(`
		SELECT text FROM turns
		WHERE session_id LIKE 'aider:%' AND role='user' AND idx=1
		ORDER BY session_id LIMIT 1`)
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Error("first user turn empty")
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
		t.Errorf("second pass added new(%d, %d); want (0, 0)", second.NewSessions, second.NewTurns)
	}
}

func TestIngestMissingRoot(t *testing.T) {
	st := openTempStore(t)
	stats, err := Ingest(context.Background(), st, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("missing root should be silent no-op, got %v", err)
	}
	if stats.RootExists {
		t.Error("RootExists should be false")
	}
}

func TestDefaultRoots(t *testing.T) {
	// We can't easily assert which dirs exist on the test machine, but
	// we CAN assert the function always returns at least one path —
	// it falls back to $HOME if no curated dir matches.
	roots := DefaultRoots()
	if len(roots) == 0 {
		t.Fatal("DefaultRoots returned empty slice")
	}
}

func TestIngestAll(t *testing.T) {
	root := setupFixtureRoot(t)
	st := openTempStore(t)

	// Run IngestAll against TWO roots — second one points at a
	// different subtree of the same fixture, so files=2 either way.
	stats, err := IngestAll(context.Background(), st, []string{root, filepath.Join(root, "code")})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.RootExists {
		t.Fatal("RootExists false")
	}
	if stats.Files < 2 {
		t.Errorf("files=%d, want >=2", stats.Files)
	}
}
