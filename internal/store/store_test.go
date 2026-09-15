package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpen_AppliesSchemaIdempotently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "x.sqlite")
	for i := 0; i < 3; i++ {
		st, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		st.Close()
	}
}

func TestInsertTurn_Idempotent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx *Tx) error {
		srcID, err := tx.UpsertSource("test", "/")
		if err != nil {
			return err
		}
		if err := tx.UpsertSession(Session{ID: "s1", SourceID: srcID, ProjectDir: "/p"}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	turn := Turn{UUID: "u1", SessionID: "s1", Idx: 1, Role: "user", Text: "hi"}
	var first, second bool
	if err := st.Tx(ctx, func(tx *Tx) error {
		var err error
		first, err = tx.InsertTurn(turn)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx *Tx) error {
		var err error
		second, err = tx.InsertTurn(turn)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("first=%v second=%v (want true,false)", first, second)
	}
	s, _ := st.Stats()
	if s.Turns != 1 {
		t.Fatalf("turns=%d want 1", s.Turns)
	}
}

func TestUpsertSession_MergesFields(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx *Tx) error {
		srcID, err := tx.UpsertSource("test", "/")
		if err != nil {
			return err
		}
		// Initial: project + start
		if err := tx.UpsertSession(Session{
			ID: "s1", SourceID: srcID, ProjectDir: "/p", StartedAt: 100, EndedAt: 200,
		}); err != nil {
			return err
		}
		// Second pass: empty project (should preserve), wider time range
		if err := tx.UpsertSession(Session{
			ID: "s1", SourceID: srcID, ProjectDir: "", StartedAt: 50, EndedAt: 300, Summary: "title",
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	row := st.DB().QueryRow(`SELECT project_dir, started_at, ended_at, summary FROM sessions WHERE id='s1'`)
	var proj, summary string
	var start, end int64
	if err := row.Scan(&proj, &start, &end, &summary); err != nil {
		t.Fatal(err)
	}
	if proj != "/p" {
		t.Errorf("project_dir=%q (should be preserved)", proj)
	}
	if start != 50 || end != 300 {
		t.Errorf("range=%d→%d want 50→300", start, end)
	}
	if summary != "title" {
		t.Errorf("summary=%q", summary)
	}
}

func TestFTS_TurnIndexed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx *Tx) error {
		srcID, _ := tx.UpsertSource("test", "/")
		_ = tx.UpsertSession(Session{ID: "s1", SourceID: srcID})
		_, _ = tx.InsertTurn(Turn{UUID: "u1", SessionID: "s1", Idx: 1, Role: "user", Text: "the quick brown fox jumps"})
		_, _ = tx.InsertTurn(Turn{UUID: "u2", SessionID: "s1", Idx: 2, Role: "user", Text: "lazy dog appears"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM turn_fts WHERE turn_fts MATCH 'fox'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("MATCH 'fox' = %d want 1", n)
	}
}
