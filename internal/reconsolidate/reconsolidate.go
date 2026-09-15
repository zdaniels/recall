// Package reconsolidate handles the supersede chain when a new decision
// contradicts (or substantially refines) an existing one in the same
// project scope.
//
// Algorithm:
//
//  1. Embed the new decision text.
//  2. Load active decisions (superseded_by IS NULL) for the same project
//     OR an ancestor scope OR global, that have an embedding.
//  3. Cosine-rank them; partition into auto-supersede (≥auto threshold),
//     candidate (≥candidate threshold), and the rest.
//  4. Mark auto-supersede rows as superseded_by the new id.
//  5. Return the candidate set so callers can surface "this looks related
//     to #N" hints without taking action.
//
// Thresholds are deliberately conservative: 0.85 for auto, 0.65 for
// candidates. NLEmbedding sentence-level scores cluster lower than
// dedicated code-trained embedders, so candidates may surface plenty of
// noise — that's why only ≥0.85 supersedes.
package reconsolidate

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zdaniels/recall/internal/embed"
	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

const (
	// reinforceThreshold: at-or-above this cosine the new text is treated as
	// a re-assertion of an existing fact (a near-duplicate), not a new one.
	// The caller reinforces the existing decision's confidence instead of
	// inserting a near-identical row — closing the trust feedback loop and
	// curbing duplicate growth at the source. Set above autoThreshold on
	// purpose: a contradiction is usually same-topic-but-reworded (high but
	// not near-identical cosine), whereas a re-assertion is near-verbatim.
	reinforceThreshold = 0.92
	autoThreshold      = 0.85
	candidateThreshold = 0.65
)

// Match describes one decision the new text relates to.
type Match struct {
	ID    int64
	Text  string
	Score float32
}

// Result is what reconsolidation produced for one new decision.
type Result struct {
	Reinforced []Match // near-duplicates (≥reinforceThreshold): re-assertions
	Superseded []Match // auto-supersede applied
	Candidates []Match // worth surfacing but not actioned
	Embedding  []byte  // encoded query vector for caller to persist
}

// Run processes a freshly-recorded decision. The caller must already have
// inserted the new decision and have its id; we read recents to find peers
// and write `superseded_by` for clear contradictions.
//
// The caller is responsible for actually persisting the returned Embedding
// onto the new decision row (we don't have its id in this signature on
// purpose — keeps the function reusable for dry-run / preview flows).
func Run(ctx context.Context, st *store.Store, emb embed.Embedder, project, text string) (*Result, error) {
	if strings.TrimSpace(text) == "" {
		return &Result{}, nil
	}
	queryVec, err := emb.Embed(ctx, text)
	if err != nil {
		// Embedding failure shouldn't block the decision write — return
		// empty result so caller proceeds without reconsolidation.
		return &Result{}, nil
	}
	encoded := vec.Encode(queryVec)

	// Load candidates: active, in-scope, with embeddings, excluding rows we
	// just inserted that have the exact same text (we don't supersede self).
	rows, err := st.DB().QueryContext(ctx, `
		SELECT id, text, embedding
		  FROM decisions
		 WHERE superseded_by IS NULL
		   AND embedding IS NOT NULL
		   AND LOWER(text) != LOWER(?)
		   AND (
		     project_dir = ?
		     OR project_dir IS NULL
		     OR ? LIKE (project_dir || '/%')
		   )`,
		text, project, project,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &Result{Embedding: encoded}
	for rows.Next() {
		var id int64
		var existingText string
		var blob []byte
		if err := rows.Scan(&id, &existingText, &blob); err != nil {
			return nil, err
		}
		v := vec.Decode(blob)
		if len(v) != len(queryVec) {
			continue
		}
		score := vec.Cosine(queryVec, v)
		switch {
		case score >= reinforceThreshold:
			res.Reinforced = append(res.Reinforced, Match{ID: id, Text: existingText, Score: score})
		case score >= autoThreshold:
			res.Superseded = append(res.Superseded, Match{ID: id, Text: existingText, Score: score})
		case score >= candidateThreshold:
			res.Candidates = append(res.Candidates, Match{ID: id, Text: existingText, Score: score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// BestReinforced returns the highest-scoring near-duplicate match, if any.
// Callers use it to reinforce an existing fact instead of inserting a
// near-identical new row.
func (r *Result) BestReinforced() (Match, bool) {
	best := Match{}
	found := false
	for _, m := range r.Reinforced {
		if !found || m.Score > best.Score {
			best, found = m, true
		}
	}
	return best, found
}

// Apply writes superseded_by for the rows in res.Superseded, against newID.
// Separated from Run so callers can choose to act / skip / dry-run.
func Apply(ctx context.Context, st *store.Store, newID int64, res *Result) error {
	if res == nil || len(res.Superseded) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(res.Superseded))
	for _, m := range res.Superseded {
		ids = append(ids, m.ID)
	}
	return st.Tx(ctx, func(tx *store.Tx) error {
		return tx.SupersedeDecisions(newID, ids)
	})
}

// silence unused-import noise for the package-internal sql import path.
var _ = sql.ErrNoRows
