package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecordFeedback(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "fb.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	var id int64
	if err := st.Tx(ctx, func(tx *Tx) error {
		var ierr error
		id, ierr = tx.InsertDecision(Decision{ProjectDir: "/p", Kind: "fact", Text: "x", Source: "cli", Salience: 1.0})
		return ierr
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Confirm twice — confidence should rise from 0.5 toward 1.0.
	c1, err := st.RecordFeedback(ctx, id, "confirmed", "test")
	if err != nil {
		t.Fatalf("confirm1: %v", err)
	}
	if c1 <= 0.5 {
		t.Fatalf("confidence did not rise: got %v", c1)
	}
	c2, err := st.RecordFeedback(ctx, id, "confirmed", "test")
	if err != nil {
		t.Fatalf("confirm2: %v", err)
	}
	if c2 <= c1 {
		t.Fatalf("confidence did not keep rising: %v -> %v", c1, c2)
	}

	// Contradict — confidence should drop.
	c3, err := st.RecordFeedback(ctx, id, "contradicted", "test")
	if err != nil {
		t.Fatalf("contradict: %v", err)
	}
	if c3 >= c2 {
		t.Fatalf("confidence did not fall on contradiction: %v -> %v", c2, c3)
	}

	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_feedback WHERE decision_id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count feedback: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 feedback rows, got %d", n)
	}
}
