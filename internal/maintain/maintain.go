// Package maintain runs the periodic housekeeping pass over the decision
// store: it merges semantic near-duplicates and ages out stale, low-value
// auto-extracted decisions. Both keep retrieval sharp as the store grows —
// the dedup pass stops monotonic duplication, the decay pass removes dead
// weight.
//
// It is scheduled by folding a once-daily gate into `recall watch` (see
// cmd/recall), and is also exposed as `recall maintain` for manual runs.
// Everything here operates on already-stored embeddings, so no embedder /
// network is required; decisions without an embedding are simply skipped by
// the dedup pass.
package maintain

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

const (
	// DefaultDedupThreshold: cosine at/above which two active decisions are
	// treated as the same fact and merged. Matches the reconsolidation
	// reinforce threshold — near-verbatim only, so we don't merge a fact
	// with its own contradiction.
	DefaultDedupThreshold = 0.92
	// DefaultDecayFloor: effective-salience below which a stale auto-extracted
	// decision is eligible for age-out.
	DefaultDecayFloor = 0.15
	// DefaultDecayDays: a decision must have gone unused (and un-created) for
	// at least this long before it can be aged out.
	DefaultDecayDays = 90
)

// decayableSources are the only `source` values the decay pass will delete —
// machine-extracted rows that re-ingestion can regenerate. User-curated
// ('cli', 'user-memory-md') and agent-asserted ('tool') decisions are never
// auto-deleted.
var decayableSources = []string{"pattern", "distilled", "surprise"}

// Options configures one maintenance run.
type Options struct {
	Project        string // "" = all projects
	DedupThreshold float64
	DecayFloor     float64
	DecayDays      int
	Dedup          bool
	Decay          bool
	DryRun         bool
}

func (o *Options) defaults() {
	if o.DedupThreshold == 0 {
		o.DedupThreshold = DefaultDedupThreshold
	}
	if o.DecayFloor == 0 {
		o.DecayFloor = DefaultDecayFloor
	}
	if o.DecayDays == 0 {
		o.DecayDays = DefaultDecayDays
	}
}

// Stats is the outcome of a maintenance run.
type Stats struct {
	Merged   int
	Decayed  int
	Duration time.Duration
}

// Run performs the configured passes. Dedup runs before decay so a merged
// row's transferred use_count / confidence can save it from the decay floor.
func Run(ctx context.Context, st *store.Store, opts Options) (Stats, error) {
	opts.defaults()
	var stats Stats
	t0 := time.Now()
	if opts.Dedup {
		n, err := dedup(ctx, st, opts)
		if err != nil {
			return stats, fmt.Errorf("dedup: %w", err)
		}
		stats.Merged = n
	}
	if opts.Decay {
		n, err := decay(ctx, st, opts)
		if err != nil {
			return stats, fmt.Errorf("decay: %w", err)
		}
		stats.Decayed = n
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

type candidate struct {
	id         int64
	project    string
	text       string
	vec        []float32
	emb        []byte
	confidence float64
	salience   float64
	ts         int64
	useCount   int64
}

// dedup merges semantic near-duplicates. Candidates are bucketed by project
// (we never merge across project scopes) and compared pairwise within each
// bucket. Within a bucket the most-trustworthy row wins (confidence, then
// salience, then recency); each weaker near-duplicate is folded into it.
func dedup(ctx context.Context, st *store.Store, opts Options) (int, error) {
	cands, err := loadCandidates(ctx, st, opts.Project)
	if err != nil {
		return 0, err
	}
	buckets := map[string][]candidate{}
	for _, c := range cands {
		buckets[c.project] = append(buckets[c.project], c)
	}

	merged := 0
	for _, bucket := range buckets {
		// Best-first so the survivor in any pair is always earlier in the slice.
		sort.SliceStable(bucket, func(i, j int) bool { return better(bucket[i], bucket[j]) })
		gone := make(map[int64]bool, len(bucket))
		for i := range bucket {
			w := bucket[i]
			if gone[w.id] {
				continue
			}
			for j := i + 1; j < len(bucket); j++ {
				l := bucket[j]
				if gone[l.id] {
					continue
				}
				if len(w.vec) != len(l.vec) {
					continue
				}
				if vec.Cosine(w.vec, l.vec) < float32(opts.DedupThreshold) {
					continue
				}
				if !opts.DryRun {
					if err := st.MergeDuplicate(ctx, w.id, l.id, l.text, l.emb); err != nil {
						return merged, err
					}
				}
				gone[l.id] = true
				merged++
			}
		}
	}
	return merged, nil
}

// better reports whether a should win over b when they're duplicates.
func better(a, b candidate) bool {
	if a.confidence != b.confidence {
		return a.confidence > b.confidence
	}
	if a.salience != b.salience {
		return a.salience > b.salience
	}
	if a.ts != b.ts {
		return a.ts > b.ts // newer wins
	}
	return a.id < b.id // stable tiebreak
}

func loadCandidates(ctx context.Context, st *store.Store, project string) ([]candidate, error) {
	q := `SELECT id, COALESCE(project_dir, ''), text, embedding,
	             COALESCE(confidence, 0.5), salience, COALESCE(ts, 0), use_count
	        FROM decisions
	       WHERE superseded_by IS NULL
	         AND embedding IS NOT NULL
	         AND kind IS NOT 'instruction'`
	args := []any{}
	if project != "" {
		q += ` AND (project_dir = ? OR ? LIKE (project_dir || '/%'))`
		args = append(args, project, project)
	}
	rows, err := st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.project, &c.text, &c.emb,
			&c.confidence, &c.salience, &c.ts, &c.useCount); err != nil {
			return nil, err
		}
		c.vec = vec.Decode(c.emb)
		out = append(out, c)
	}
	return out, rows.Err()
}

// decay deletes stale, low-salience, machine-extracted decisions. The filter
// is deliberately conservative: only decayableSources, never instructions,
// only rows whose effective salience has fallen below the floor AND that
// haven't been used or created within the decay window.
func decay(ctx context.Context, st *store.Store, opts Options) (int, error) {
	cutoff := time.Now().AddDate(0, 0, -opts.DecayDays).Unix()
	placeholders := ""
	args := []any{}
	for i, s := range decayableSources {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, s)
	}
	args = append(args, cutoff)
	q := `SELECT id FROM decisions
	       WHERE superseded_by IS NULL
	         AND kind IS NOT 'instruction'
	         AND source IN (` + placeholders + `)
	         AND COALESCE(last_used_at, ts, 0) < ?
	         AND ` + store.EffectiveSalienceExpr + ` < ?`
	args = append(args, opts.DecayFloor)
	if opts.Project != "" {
		q += ` AND (project_dir = ? OR ? LIKE (project_dir || '/%'))`
		args = append(args, opts.Project, opts.Project)
	}
	rows, err := st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if opts.DryRun {
		return len(ids), nil
	}
	n, err := st.DeleteDecisions(ctx, ids)
	return int(n), err
}
