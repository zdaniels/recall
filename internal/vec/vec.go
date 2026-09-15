// Package vec implements pure-Go vector storage + cosine similarity for
// semantic recall.
//
// We deliberately do NOT load the sqlite-vec C extension. Reasons:
//
//  1. modernc.org/sqlite is pure Go; loading C extensions reintroduces CGO
//     and breaks the single-binary-cross-compile property.
//  2. At our scale (a few thousand decisions × 768 dims) a linear cosine
//     scan in Go is well under 50ms — sqlite-vec's HNSW indexing buys
//     orders of magnitude that we don't need yet.
//
// Embeddings are stored as little-endian float32 BLOBs in the `embedding`
// columns added in schema v2. Encode/Decode round-trip; Cosine returns the
// standard cosine similarity in [-1, 1].
package vec

import (
	"encoding/binary"
	"math"
	"sort"
)

// Encode serialises a float32 vector to little-endian bytes (4 bytes/elem).
func Encode(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// Decode is Encode's inverse. Returns nil for nil/empty input.
func Decode(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// Cosine returns cosine similarity between a and b. Returns 0 if either
// vector is empty, mismatched, or zero-norm.
func Cosine(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ax := float64(a[i])
		bx := float64(b[i])
		dot += ax * bx
		na += ax * ax
		nb += bx * bx
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// Hit is one ranked result from TopK.
type Hit struct {
	ID    int64
	Score float32
}

// TopK ranks candidates against query by cosine similarity and returns the
// top-k. Stable, deterministic on ties (lower ID wins).
func TopK(query []float32, candidates map[int64][]float32, k int) []Hit {
	if len(query) == 0 || len(candidates) == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(candidates))
	for id, vec := range candidates {
		hits = append(hits, Hit{ID: id, Score: Cosine(query, vec)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
