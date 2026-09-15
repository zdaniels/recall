package recall

import "sort"

// Reciprocal Rank Fusion combines several ranked lists into one ranking.
//
// For each candidate document d, the fused score is:
//
//	score(d) = Σ over channels  1 / (k + rank_i(d))
//
// where rank_i(d) is d's 1-indexed rank in channel i (∞ if absent), and
// k is a small integer (typically 60) that tempers the contribution of
// top-1 hits. Standard, parameter-light, robust against channels with
// wildly different score scales (FTS rank vs cosine similarity).
//
// References: Cormack et al, "Reciprocal Rank Fusion outperforms Condorcet
// and individual Rank Learning Methods" (SIGIR 2009). Standard hybrid-search
// fusion technique; used in Vespa, Elasticsearch RRF, and many others.

// DefaultK is the standard RRF damping constant.
const DefaultK = 60

// Hit is one item in a ranked list. ID is opaque to RRF (callers use
// namespaced strings like "turn:<uuid>" or "dec:10" so a single RRF
// call can fuse heterogeneous entities). Score is the source channel's
// raw score, kept for tie-breaking and inspection but NOT used in the
// fused calculation (RRF is rank-based by design).
type Hit struct {
	ID    string
	Score float64
	// Channels is populated by Fuse: list of channel indices that
	// surfaced this hit. Useful for "which retrievers found this?".
	Channels []int
}

// Channel is a single ranker's output, ordered most-relevant first.
type Channel []Hit

// Fuse merges N ranked channels via Reciprocal Rank Fusion. K is the
// damping constant (use DefaultK for standard behaviour). limit caps
// the output size; <= 0 means "all".
//
// Stable: when two documents have identical fused scores, the one with
// the earlier appearance in the first channel that ranks them wins.
func Fuse(channels []Channel, k int, limit int) []Hit {
	return FuseWeighted(channels, nil, k, limit)
}

// FuseWeighted is Fuse with a per-channel weight on each contribution:
//
//	score(d) = Σ over channels  weight_i / (k + rank_i(d))
//
// weights[i] applies to channels[i]; nil or shorter than channels gives
// 1.0 for any unsupplied channel (so the caller can pass nil for plain
// RRF). A weight of 0 effectively excludes the channel without changing
// the channels slice. Negative weights are clamped to 0.
func FuseWeighted(channels []Channel, weights []float64, k int, limit int) []Hit {
	if k <= 0 {
		k = DefaultK
	}
	weightAt := func(i int) float64 {
		if i < len(weights) {
			if weights[i] < 0 {
				return 0
			}
			return weights[i]
		}
		return 1.0
	}
	scores := map[string]float64{}
	firstSeen := map[string]int{}     // earliest channel rank, for stable tie-break
	channelsHit := map[string][]int{} // which channels surfaced each id
	for chIdx, ch := range channels {
		w := weightAt(chIdx)
		if w == 0 {
			continue
		}
		for rank, h := range ch {
			scores[h.ID] += w / float64(k+rank+1)
			if _, ok := firstSeen[h.ID]; !ok {
				firstSeen[h.ID] = chIdx*1_000_000 + rank
			}
			channelsHit[h.ID] = append(channelsHit[h.ID], chIdx)
		}
	}
	out := make([]Hit, 0, len(scores))
	for id, s := range scores {
		out = append(out, Hit{ID: id, Score: s, Channels: channelsHit[id]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return firstSeen[out[i].ID] < firstSeen[out[j].ID]
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
