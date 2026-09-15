package maintain

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

// seed inserts a decision with an embedding and returns its id.
func seed(t *testing.T, st *store.Store, d store.Decision, v []float32) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = tx.InsertDecision(d)
		if err != nil {
			return err
		}
		return tx.SetEmbedding(id, vec.Encode(v))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func active(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM decisions WHERE superseded_by IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestDedupMergesNearDuplicates(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Two near-identical vectors (same project) → should merge to one.
	win := seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "primary db is postgres", Source: "cli", Salience: 2.0}, []float32{1, 0, 0})
	_ = seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "postgres is the primary db", Source: "cli", Salience: 1.0}, []float32{0.99, 0.05, 0})
	// An unrelated one (same project) → untouched.
	_ = seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "we deploy on fridays", Source: "cli", Salience: 1.0}, []float32{0, 1, 0})
	// A near-duplicate but in a DIFFERENT project → must NOT merge across scopes.
	_ = seed(t, st, store.Decision{ProjectDir: "/other", Kind: "fact", Text: "primary db is postgres", Source: "cli", Salience: 1.0}, []float32{1, 0, 0})

	if got := active(t, st); got != 4 {
		t.Fatalf("precondition: want 4 active, got %d", got)
	}

	stats, err := Run(ctx, st, Options{Dedup: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Merged != 1 {
		t.Fatalf("want 1 merge, got %d", stats.Merged)
	}
	if got := active(t, st); got != 3 {
		t.Fatalf("want 3 active after merge, got %d", got)
	}

	// Higher-salience row must be the survivor, and it should have absorbed a
	// paraphrase + a confidence bump.
	var conf float64
	var paras int
	if err := st.DB().QueryRow(`SELECT confidence FROM decisions WHERE id=?`, win).Scan(&conf); err != nil {
		t.Fatalf("winner: %v", err)
	}
	if conf <= 0.5 {
		t.Fatalf("winner confidence not bumped: %v", conf)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM decision_paraphrases WHERE decision_id=?`, win).Scan(&paras); err != nil {
		t.Fatalf("paras: %v", err)
	}
	if paras != 1 {
		t.Fatalf("want loser text kept as 1 paraphrase, got %d", paras)
	}
}

func TestDecayAgesOutStaleAutoRows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	old := time.Now().AddDate(0, 0, -200).Unix()
	recent := time.Now().Unix()

	// Stale, low-salience, pattern-source → eligible.
	_ = seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "stale pattern row", Source: "pattern", Salience: 0.2, Ts: old}, []float32{1, 0, 0})
	// Stale but user-curated (cli) → protected.
	keepCLI := seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "stale but curated", Source: "cli", Salience: 0.2, Ts: old}, []float32{0, 1, 0})
	// Stale instruction → protected.
	keepInstr := seed(t, st, store.Decision{ProjectDir: "/p", Kind: "instruction", Text: "stale runbook", Source: "pattern", Salience: 0.2, Ts: old}, []float32{0, 0, 1})
	// Recent pattern row → not old enough.
	keepRecent := seed(t, st, store.Decision{ProjectDir: "/p", Kind: "fact", Text: "fresh pattern row", Source: "pattern", Salience: 0.2, Ts: recent}, []float32{1, 1, 0})

	// Dry-run first: should report 1 without deleting.
	dry, err := Run(ctx, st, Options{Decay: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Decayed != 1 {
		t.Fatalf("dry-run want 1 eligible, got %d", dry.Decayed)
	}
	if got := active(t, st); got != 4 {
		t.Fatalf("dry-run must not delete; want 4, got %d", got)
	}

	stats, err := Run(ctx, st, Options{Decay: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Decayed != 1 {
		t.Fatalf("want 1 decayed, got %d", stats.Decayed)
	}
	for _, id := range []int64{keepCLI, keepInstr, keepRecent} {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM decisions WHERE id=?`, id).Scan(&n); err != nil {
			t.Fatalf("check %d: %v", id, err)
		}
		if n != 1 {
			t.Fatalf("decision #%d should have been protected from decay", id)
		}
	}
}

func TestMaintainMarkerRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "k.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if got, err := st.LastMaintainAt(ctx); err != nil || !got.IsZero() {
		t.Fatalf("want zero time initially, got %v err=%v", got, err)
	}
	now := time.Now().Truncate(time.Second)
	if err := st.SetMaintainAt(ctx, now); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.LastMaintainAt(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Equal(now) {
		t.Fatalf("round-trip mismatch: set %v got %v", now, got)
	}
}
