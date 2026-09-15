package reconsolidate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

// stubEmbedder returns a fixed vector per exact text — lets us drive cosine
// scores deterministically without the ONNX model.
type stubEmbedder struct{ vecs map[string][]float32 }

func (s stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return s.vecs[text], nil
}
func (s stubEmbedder) Name() string { return "stub" }
func (s stubEmbedder) Dim() int     { return 3 }

func TestRunBuckets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Existing decisions with controlled embeddings (unit vectors in 3-space).
	// Cosine to the query [1,0,0]: reinforce≈0.99, supersede≈0.88, ignore=0.
	existing := []struct {
		text string
		v    []float32
	}{
		{"reinforce-me", []float32{0.99, 0.14, 0}},  // ≥0.92 → Reinforced
		{"supersede-me", []float32{0.88, 0.475, 0}}, // 0.85–0.92 → Superseded
		{"unrelated", []float32{0, 1, 0}},           // <0.65 → ignored
	}
	ids := map[string]int64{}
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		for _, e := range existing {
			id, err := tx.InsertDecision(store.Decision{ProjectDir: "/p", Kind: "fact", Text: e.text, Source: "cli"})
			if err != nil {
				return err
			}
			if err := tx.SetEmbedding(id, vec.Encode(e.v)); err != nil {
				return err
			}
			ids[e.text] = id
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	emb := stubEmbedder{vecs: map[string][]float32{"query-text": {1, 0, 0}}}
	res, err := Run(ctx, st, emb, "/p", "query-text")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(res.Reinforced) != 1 || res.Reinforced[0].ID != ids["reinforce-me"] {
		t.Fatalf("expected reinforce-me in Reinforced, got %+v", res.Reinforced)
	}
	if len(res.Superseded) != 1 || res.Superseded[0].ID != ids["supersede-me"] {
		t.Fatalf("expected supersede-me in Superseded, got %+v", res.Superseded)
	}
	if best, ok := res.BestReinforced(); !ok || best.ID != ids["reinforce-me"] {
		t.Fatalf("BestReinforced wrong: %+v ok=%v", best, ok)
	}
}
