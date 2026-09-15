package vec

import (
	"math"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	v := []float32{0, 1, -1, 0.5, -0.25, 3.14159}
	got := Decode(Encode(v))
	if len(got) != len(v) {
		t.Fatalf("len=%d want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("[%d] %f != %f", i, got[i], v[i])
		}
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1.0},
		{[]float32{1, 0}, []float32{0, 1}, 0.0},
		{[]float32{1, 0}, []float32{-1, 0}, -1.0},
		{[]float32{1, 1}, []float32{1, 1}, 1.0},
	}
	for _, c := range cases {
		got := Cosine(c.a, c.b)
		if math.Abs(float64(got)-c.want) > 1e-6 {
			t.Errorf("Cosine(%v, %v) = %f, want %f", c.a, c.b, got, c.want)
		}
	}
}

func TestCosine_EdgeCases(t *testing.T) {
	if got := Cosine(nil, nil); got != 0 {
		t.Errorf("nil/nil = %f, want 0", got)
	}
	if got := Cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Errorf("zero-norm = %f, want 0", got)
	}
	if got := Cosine([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("len mismatch should = 0, got %f", got)
	}
}

func TestTopK(t *testing.T) {
	q := []float32{1, 0}
	cands := map[int64][]float32{
		1: {1, 0},      // perfect match
		2: {0.99, 0.1}, // very close
		3: {0, 1},      // orthogonal
		4: {-1, 0},     // opposite
	}
	hits := TopK(q, cands, 2)
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].ID != 1 || hits[1].ID != 2 {
		t.Errorf("ranking wrong: %+v", hits)
	}
}
