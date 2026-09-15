// Package bench runs retrieval benchmarks against a recall store.
//
// LongMemEval is the question-answer-haystack format defined in
// arxiv.org/abs/2410.10813. Each question carries its own corpus of
// synthetic conversation sessions plus the ids of the session(s) that
// hold the answer; a memory system is scored on whether one of those
// answer sessions appears in the top-K of its retrieval. We run mode=
// hybrid (FTS5 over turn_fts + cosine over turn embeddings + temporal
// recency, fused via RRF) and report R@1 / R@5 / R@10. No LLM rerank.
//
// The bench mutates a temp SQLite via store.Open. Between questions we
// truncate sessions/turns/sources/turn_fts to start with a clean haystack
// (faster than recreating the file per question; same effect).
package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/recall"
	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

// Question is the subset of a LongMemEval question we read. The full
// schema has more fields (question_type, question_date, ...); we ignore
// those.
//
// The published cleaned dataset uses parallel arrays for the haystack:
// haystack_session_ids[i] names the session whose messages live in
// haystack_sessions[i] and whose timestamp is haystack_dates[i]. Older
// or alternate forms (a dict {sid: [msgs]} or an array of
// {session_id, messages} objects) also exist in the wild; parseHaystack
// accepts all three.
type question struct {
	QuestionID         string          `json:"question_id"`
	Question           string          `json:"question"`
	HaystackSessions   json.RawMessage `json:"haystack_sessions"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	AnswerSessionIDs   []string        `json:"answer_session_ids"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Ts      int64  `json:"timestamp,omitempty"`
}

// Result is the aggregate output of a benchmark run.
type Result struct {
	Total       int              `json:"total"`
	HitsAt1     int              `json:"hits_at_1"`
	HitsAt5     int              `json:"hits_at_5"`
	HitsAt10    int              `json:"hits_at_10"`
	Mode        string           `json:"mode"`
	Embedder    string           `json:"embedder"`
	Elapsed     time.Duration    `json:"elapsed_ns"`
	PerQuestion []QuestionResult `json:"per_question,omitempty"`
}

func (r Result) RecallAt(k int) float64 {
	if r.Total == 0 {
		return 0
	}
	switch k {
	case 1:
		return float64(r.HitsAt1) / float64(r.Total)
	case 5:
		return float64(r.HitsAt5) / float64(r.Total)
	case 10:
		return float64(r.HitsAt10) / float64(r.Total)
	}
	return 0
}

// QuestionResult is the per-question outcome (top-K session ids and which
// rank, if any, contained the answer). Captured only when Options.Verbose.
type QuestionResult struct {
	QuestionID  string   `json:"question_id"`
	TopSessions []string `json:"top_sessions"`
	AnswerIDs   []string `json:"answer_ids"`
	HitAtRank   int      `json:"hit_at_rank"` // 0 = miss; otherwise 1-indexed
}

// Options configures one benchmark run.
type Options struct {
	DataPath string          // path to longmemeval_*.json
	DBPath   string          // working SQLite (will be truncated per-question)
	Embedder recall.Embedder // for semantic channel; nil = lexical+temporal only
	Limit    int             // cap questions evaluated; 0 = all
	Skip     int             // skip the first Skip questions before evaluating (for held-out splits)
	Verbose  bool            // include per-question results
	Progress io.Writer       // optional, prints "%d/%d done" lines

	// Retrieval knobs. RRFk = 0 → recall.DefaultK. Weights = nil → all 1.0
	// (plain RRF). Channel order is [lex, sem, temporal, keyword].
	RRFk    int
	Weights []float64

	// Rerank, when true and ANTHROPIC_API_KEY is set, runs a Haiku
	// rerank pass over the top-RerankTopK fused candidates per question.
	// Adds ~0.5–2s and ~$0.001 per question; gracefully no-ops when no
	// API key is available.
	Rerank     bool
	RerankTopK int
}

// Run executes the benchmark and returns aggregated R@k.
func Run(ctx context.Context, opts Options) (Result, error) {
	t0 := time.Now()

	raw, err := os.ReadFile(opts.DataPath)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", opts.DataPath, err)
	}
	var qs []question
	if err := json.Unmarshal(raw, &qs); err != nil {
		return Result{}, fmt.Errorf("parse longmemeval json: %w", err)
	}
	if opts.Skip > 0 {
		if opts.Skip >= len(qs) {
			qs = nil
		} else {
			qs = qs[opts.Skip:]
		}
	}
	if opts.Limit > 0 && opts.Limit < len(qs) {
		qs = qs[:opts.Limit]
	}

	st, err := store.Open(opts.DBPath)
	if err != nil {
		return Result{}, fmt.Errorf("open bench db: %w", err)
	}
	defer st.Close()

	mode := "lexical+temporal"
	embedderName := ""
	if opts.Embedder != nil {
		mode = "hybrid (lexical+semantic+temporal)"
		embedderName = opts.Embedder.Name()
	}
	res := Result{Total: len(qs), Mode: mode, Embedder: embedderName}

	for i, q := range qs {
		if err := resetBenchDB(ctx, st.DB()); err != nil {
			return res, fmt.Errorf("reset db: %w", err)
		}

		sessions, err := parseHaystack(q.HaystackSessions, q.HaystackSessionIDs)
		if err != nil {
			return res, fmt.Errorf("question %s: %w", q.QuestionID, err)
		}
		if err := loadHaystack(ctx, st, sessions, opts.Embedder); err != nil {
			return res, fmt.Errorf("ingest haystack for %s: %w", q.QuestionID, err)
		}

		hits, err := searchBench(ctx, st.DB(), q.Question, opts.Embedder, 10, opts.RRFk, opts.Weights)
		if err != nil {
			return res, fmt.Errorf("search %s: %w", q.QuestionID, err)
		}
		if opts.Rerank && len(hits) > 1 {
			topK := opts.RerankTopK
			if topK <= 0 {
				topK = 5
			}
			if topK > len(hits) {
				topK = len(hits)
			}
			texts, terr := fetchTurnTexts(ctx, st.DB(), hits[:topK])
			if terr != nil {
				return res, fmt.Errorf("rerank fetch %s: %w", q.QuestionID, terr)
			}
			pick, perr := recall.PickBestExcerpt(ctx, q.Question, texts, recall.RerankOptions{TopK: topK})
			if perr != nil {
				// Don't kill the whole run on a transient API hiccup; log and
				// fall through with the original order. We still record the
				// question's hit so a few failed reranks don't bias the score.
				fmt.Fprintf(os.Stderr, "warn: rerank %s: %v\n", q.QuestionID, perr)
			} else if pick > 1 && pick <= topK {
				chosen := hits[pick-1]
				copy(hits[1:pick], hits[0:pick-1])
				hits[0] = chosen
			}
		}

		topSessions := uniqSessions(hits)
		answerSet := map[string]bool{}
		for _, a := range q.AnswerSessionIDs {
			answerSet[a] = true
		}
		hitRank := 0
		for r, sid := range topSessions {
			if answerSet[sid] {
				hitRank = r + 1
				break
			}
		}
		switch {
		case hitRank > 0 && hitRank <= 1:
			res.HitsAt1++
			res.HitsAt5++
			res.HitsAt10++
		case hitRank > 0 && hitRank <= 5:
			res.HitsAt5++
			res.HitsAt10++
		case hitRank > 0 && hitRank <= 10:
			res.HitsAt10++
		}

		if opts.Verbose {
			res.PerQuestion = append(res.PerQuestion, QuestionResult{
				QuestionID: q.QuestionID, TopSessions: topSessions,
				AnswerIDs: q.AnswerSessionIDs, HitAtRank: hitRank,
			})
		}
		if opts.Progress != nil && (i+1)%10 == 0 {
			fmt.Fprintf(opts.Progress, "  %d/%d  R@5=%.1f%%\n",
				i+1, len(qs), 100*float64(res.HitsAt5)/float64(i+1))
		}
	}

	res.Elapsed = time.Since(t0)
	return res, nil
}

// resetBenchDB truncates ingested-data tables between questions. We keep
// schema_version so migrations don't re-run.
func resetBenchDB(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		"DELETE FROM turns",
		"DELETE FROM sessions",
		"DELETE FROM sources",
		"DELETE FROM files",
		"DELETE FROM tool_events",
		"DELETE FROM ingest_state",
		"DELETE FROM turn_fts",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

type haystackSession struct {
	ID       string
	Messages []message
}

// parseHaystack accepts the three shapes that show up in the wild:
//
//  1. Parallel arrays: `haystack_sessions` is `[[msg, msg, ...], ...]`
//     and `haystack_session_ids` is the same length, naming each entry.
//     This is the form used by the published cleaned dataset.
//  2. Dict: `haystack_sessions` is `{session_id: [msgs]}`.
//  3. Array of objects: `haystack_sessions` is
//     `[{session_id, messages: [...]}, ...]`.
//
// Tolerant of unknown fields. `parallelIDs` is consulted only for shape (1).
func parseHaystack(raw json.RawMessage, parallelIDs []string) ([]haystackSession, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))

	if strings.HasPrefix(trimmed, "{") {
		var dict map[string][]message
		if err := json.Unmarshal(raw, &dict); err != nil {
			return nil, fmt.Errorf("haystack object form: %w", err)
		}
		out := make([]haystackSession, 0, len(dict))
		for sid, msgs := range dict {
			out = append(out, haystackSession{ID: sid, Messages: msgs})
		}
		// Stable order so behaviour is reproducible.
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}

	if !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("haystack_sessions: unrecognised JSON shape")
	}

	// Two array forms differ by element type: a list of message-lists
	// (parallel-arrays form) vs. a list of {session_id, messages} objects.
	// Try the parallel-arrays form first since that's what the cleaned
	// dataset ships.
	var nested [][]message
	if err := json.Unmarshal(raw, &nested); err == nil {
		out := make([]haystackSession, 0, len(nested))
		for i, msgs := range nested {
			id := fmt.Sprintf("session-%d", i)
			if i < len(parallelIDs) && parallelIDs[i] != "" {
				id = parallelIDs[i]
			}
			out = append(out, haystackSession{ID: id, Messages: msgs})
		}
		return out, nil
	}

	var arr []struct {
		SessionID string    `json:"session_id"`
		ID        string    `json:"id"`
		Messages  []message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("haystack array form: %w", err)
	}
	out := make([]haystackSession, 0, len(arr))
	for i, s := range arr {
		id := s.SessionID
		if id == "" {
			id = s.ID
		}
		if id == "" {
			id = fmt.Sprintf("session-%d", i)
		}
		out = append(out, haystackSession{ID: id, Messages: s.Messages})
	}
	return out, nil
}

// loadHaystack writes the question's haystack into the bench DB. One row
// in sources/sessions per session, one in turns per message. Embeds turn
// text on the way in if Embedder != nil so the cosine channel can fire.
func loadHaystack(ctx context.Context, st *store.Store, sessions []haystackSession, embedder recall.Embedder) error {
	return st.Tx(ctx, func(tx *store.Tx) error {
		sourceID, err := tx.UpsertSource("longmemeval-bench", "/bench/longmemeval")
		if err != nil {
			return err
		}
		for _, s := range sessions {
			if err := tx.UpsertSession(store.Session{
				ID: s.ID, SourceID: sourceID,
				StartedAt: 0, EndedAt: 0,
				Summary: "",
			}); err != nil {
				return fmt.Errorf("upsert session %s: %w", s.ID, err)
			}
			for i, m := range s.Messages {
				inserted, err := tx.InsertTurn(store.Turn{
					UUID:      fmt.Sprintf("%s:%d", s.ID, i),
					SessionID: s.ID, Idx: i,
					Role: m.Role, Ts: m.Ts, Text: m.Content,
				})
				if err != nil {
					return fmt.Errorf("insert turn %s/%d: %w", s.ID, i, err)
				}
				if !inserted || embedder == nil || strings.TrimSpace(m.Content) == "" {
					continue
				}
				v, err := embedder.Embed(ctx, m.Content)
				if err != nil {
					return fmt.Errorf("embed turn %s/%d: %w", s.ID, i, err)
				}
				if err := tx.SetTurnEmbedding(fmt.Sprintf("%s:%d", s.ID, i), vec.Encode(v)); err != nil {
					return fmt.Errorf("save turn embedding: %w", err)
				}
			}
		}
		return nil
	})
}

// searchBench runs FTS5 over turn_fts + cosine over turn embeddings (when
// embedder available) + temporal re-rank over the union, fused via RRF.
// Returns turn-level hits ordered by fused score.
type benchHit struct {
	TurnUUID  string
	SessionID string
}

func searchBench(ctx context.Context, db *sql.DB, query string, embedder recall.Embedder, limit int, rrfk int, weights []float64) ([]benchHit, error) {
	// Large candidate pool gives RRF more overlap to find consensus across
	// channels — crucial when one channel ranks the right session at #15
	// and another at #20: a smaller pool would miss it entirely.
	pool := 100
	if limit*4 > pool {
		pool = limit * 4
	}

	// FTS5 channel — quoted-tokens sanitization re-uses the same logic as
	// production; we inline the filter here to avoid exporting it.
	safe := sanitizeFTS5(query)
	lex := recall.Channel{}
	if safe != "" {
		rows, err := db.QueryContext(ctx, `
			SELECT turns.uuid, turns.session_id
			FROM turn_fts
			JOIN turns ON turn_fts.rowid = turns.rowid
			WHERE turn_fts MATCH ?
			ORDER BY rank
			LIMIT ?`, safe, pool)
		if err != nil {
			return nil, fmt.Errorf("fts: %w", err)
		}
		i := 0
		for rows.Next() {
			var uuid, sid string
			if err := rows.Scan(&uuid, &sid); err != nil {
				rows.Close()
				return nil, err
			}
			lex = append(lex, recall.Hit{ID: "turn:" + uuid, Score: 1.0 / float64(i+1)})
			i++
		}
		rows.Close()
	}

	// Cosine channel over turn embeddings
	sem := recall.Channel{}
	if embedder != nil {
		qv, err := embedder.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		rows, err := db.QueryContext(ctx, `SELECT uuid, embedding FROM turns WHERE embedding IS NOT NULL`)
		if err != nil {
			return nil, fmt.Errorf("turn embeddings: %w", err)
		}
		type scored struct {
			uuid string
			s    float32
		}
		all := make([]scored, 0, 256)
		for rows.Next() {
			var uuid string
			var blob []byte
			if err := rows.Scan(&uuid, &blob); err != nil {
				rows.Close()
				return nil, err
			}
			tv := vec.Decode(blob)
			if len(tv) == 0 {
				continue
			}
			all = append(all, scored{uuid: uuid, s: vec.Cosine(qv, tv)})
		}
		rows.Close()
		sort.SliceStable(all, func(i, j int) bool { return all[i].s > all[j].s })
		if len(all) > pool {
			all = all[:pool]
		}
		for _, r := range all {
			sem = append(sem, recall.Hit{ID: "turn:" + r.uuid, Score: float64(r.s)})
		}
	}

	// Temporal channel over union by recency (uses the same SQL helper
	// as production — but here turns may have ts=0, in which case ranks
	// fall to the bottom of this channel without breaking RRF math).
	temp := recall.Channel{}
	{
		ids := unionIDs(lex, sem)
		if len(ids) > 0 {
			placeholders := strings.Repeat("?,", len(ids))
			placeholders = placeholders[:len(placeholders)-1]
			args := make([]any, 0, len(ids))
			for _, id := range ids {
				args = append(args, strings.TrimPrefix(id, "turn:"))
			}
			rows, err := db.QueryContext(ctx,
				fmt.Sprintf(`SELECT uuid, COALESCE(ts, 0) FROM turns WHERE uuid IN (%s)`, placeholders),
				args...)
			if err != nil {
				return nil, fmt.Errorf("temporal: %w", err)
			}
			tsMap := map[string]int64{}
			for rows.Next() {
				var uuid string
				var ts int64
				if err := rows.Scan(&uuid, &ts); err != nil {
					rows.Close()
					return nil, err
				}
				tsMap[uuid] = ts
			}
			rows.Close()
			type tsHit struct {
				id string
				ts int64
			}
			tsHits := make([]tsHit, 0, len(ids))
			for _, id := range ids {
				tsHits = append(tsHits, tsHit{id: id, ts: tsMap[strings.TrimPrefix(id, "turn:")]})
			}
			sort.SliceStable(tsHits, func(i, j int) bool { return tsHits[i].ts > tsHits[j].ts })
			for _, h := range tsHits {
				temp = append(temp, recall.Hit{ID: h.id, Score: float64(h.ts)})
			}
		}
	}

	// Keyword-overlap channel over the union: count distinct content-word
	// hits in each candidate's text. FTS5's bm25 saturates fast, so a hit
	// containing 6 of 8 query keywords ranks identically to one with 4 of
	// 8. This channel re-orders the same candidate set strictly by
	// coverage, tie-breaking by length so concise matches win.
	kw := recall.Channel{}
	{
		ids := unionIDs(lex, sem)
		if len(ids) > 0 && len(query) > 0 {
			keywords := contentWords(query)
			if len(keywords) > 0 {
				placeholders := strings.Repeat("?,", len(ids))
				placeholders = placeholders[:len(placeholders)-1]
				args := make([]any, 0, len(ids))
				for _, id := range ids {
					args = append(args, strings.TrimPrefix(id, "turn:"))
				}
				rows, err := db.QueryContext(ctx,
					fmt.Sprintf(`SELECT uuid, text FROM turns WHERE uuid IN (%s)`, placeholders),
					args...)
				if err != nil {
					return nil, fmt.Errorf("keyword: %w", err)
				}
				type kwHit struct {
					id      string
					hits    int
					textLen int
				}
				scored := make([]kwHit, 0, len(ids))
				for rows.Next() {
					var uuid, text string
					if err := rows.Scan(&uuid, &text); err != nil {
						rows.Close()
						return nil, err
					}
					low := strings.ToLower(text)
					n := 0
					for _, k := range keywords {
						if strings.Contains(low, k) {
							n++
						}
					}
					scored = append(scored, kwHit{id: "turn:" + uuid, hits: n, textLen: len(text)})
				}
				rows.Close()
				sort.SliceStable(scored, func(i, j int) bool {
					if scored[i].hits != scored[j].hits {
						return scored[i].hits > scored[j].hits
					}
					return scored[i].textLen < scored[j].textLen
				})
				for _, s := range scored {
					if s.hits == 0 {
						break
					}
					kw = append(kw, recall.Hit{ID: s.id, Score: float64(s.hits)})
				}
			}
		}
	}

	channels := []recall.Channel{lex, sem, temp, kw}
	k := rrfk
	if k <= 0 {
		k = recall.DefaultK
	}
	fused := recall.FuseWeighted(channels, weights, k, pool)

	// Map turn → session for the score, in fused order.
	out := make([]benchHit, 0, len(fused))
	if len(fused) == 0 {
		return out, nil
	}
	uuids := make([]any, 0, len(fused))
	uuidIndex := map[string]int{}
	for i, h := range fused {
		u := strings.TrimPrefix(h.ID, "turn:")
		uuids = append(uuids, u)
		uuidIndex[u] = i
	}
	placeholders := strings.Repeat("?,", len(uuids))
	placeholders = placeholders[:len(placeholders)-1]
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT uuid, session_id FROM turns WHERE uuid IN (%s)`, placeholders),
		uuids...)
	if err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}
	defer rows.Close()
	uuidToSession := map[string]string{}
	for rows.Next() {
		var uuid, sid string
		if err := rows.Scan(&uuid, &sid); err != nil {
			return nil, err
		}
		uuidToSession[uuid] = sid
	}
	for _, h := range fused {
		u := strings.TrimPrefix(h.ID, "turn:")
		out = append(out, benchHit{TurnUUID: u, SessionID: uuidToSession[u]})
	}
	return out, nil
}

// fetchTurnTexts pulls the textual content for a slice of bench hits in
// hit-order. Used to feed the rerank model — it sees the same excerpts
// the human would, in the same order RRF chose, so we can ask "which
// of these N answers the query?".
func fetchTurnTexts(ctx context.Context, db *sql.DB, hits []benchHit) ([]string, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	uuids := make([]any, 0, len(hits))
	for _, h := range hits {
		uuids = append(uuids, h.TurnUUID)
	}
	placeholders := strings.Repeat("?,", len(uuids))
	placeholders = placeholders[:len(placeholders)-1]
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT uuid, text FROM turns WHERE uuid IN (%s)`, placeholders),
		uuids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	textByUUID := map[string]string{}
	for rows.Next() {
		var uuid, text string
		if err := rows.Scan(&uuid, &text); err != nil {
			return nil, err
		}
		textByUUID[uuid] = text
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = textByUUID[h.TurnUUID]
	}
	return out, nil
}

func uniqSessions(hits []benchHit) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.SessionID == "" || seen[h.SessionID] {
			continue
		}
		seen[h.SessionID] = true
		out = append(out, h.SessionID)
	}
	return out
}

func unionIDs(channels ...recall.Channel) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, ch := range channels {
		for _, h := range ch {
			if seen[h.ID] {
				continue
			}
			seen[h.ID] = true
			out = append(out, h.ID)
		}
	}
	return out
}

// sanitizeFTS5 mirrors the production sanitizer (internal/recall.sanitizeFTS5)
// — wrapping tokens with FTS5 meta chars in double quotes so natural-
// language queries don't trigger column-resolution errors. Kept local so
// the bench package is self-contained.
func sanitizeFTS5(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if isSimpleFTSToken(f) {
			out = append(out, f)
			continue
		}
		escaped := strings.ReplaceAll(f, `"`, `""`)
		out = append(out, `"`+escaped+`"`)
	}
	return strings.Join(out, " ")
}

func isSimpleFTSToken(s string) bool {
	if s == "" {
		return false
	}
	end := len(s)
	if s[end-1] == '*' {
		end--
	}
	if end == 0 {
		return false
	}
	for i := 0; i < end; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
		default:
			return false
		}
	}
	return true
}

// FormatHuman renders the result for terminal output.
func FormatHuman(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LongMemEval bench: %d questions\n", r.Total)
	fmt.Fprintf(&b, "  R@1:  %5.1f%%  (%d / %d)\n", 100*r.RecallAt(1), r.HitsAt1, r.Total)
	fmt.Fprintf(&b, "  R@5:  %5.1f%%  (%d / %d)\n", 100*r.RecallAt(5), r.HitsAt5, r.Total)
	fmt.Fprintf(&b, "  R@10: %5.1f%%  (%d / %d)\n", 100*r.RecallAt(10), r.HitsAt10, r.Total)
	fmt.Fprintf(&b, "mode: %s\n", r.Mode)
	if r.Embedder != "" {
		fmt.Fprintf(&b, "embedder: %s\n", r.Embedder)
	}
	fmt.Fprintf(&b, "elapsed: %s\n", r.Elapsed.Round(time.Millisecond))
	return b.String()
}

// Avoid unused-import warning when math isn't reached in trim builds.
var _ = math.Inf

// stopwords are dropped before keyword-overlap counting. Curated to be
// small and English-specific — the goal is to keep "deploy", "auth",
// "postgres", drop "what", "the", "is". Accuracy over completeness:
// missing one obvious stopword hurts a few questions; including a real
// content word as a stopword could hurt many.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {}, "if": {},
	"of": {}, "in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "from": {},
	"by": {}, "with": {}, "as": {}, "about": {}, "into": {}, "over": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"have": {}, "has": {}, "had": {}, "do": {}, "does": {}, "did": {}, "doing": {},
	"will": {}, "would": {}, "could": {}, "should": {}, "may": {}, "might": {},
	"can": {}, "i": {}, "you": {}, "we": {}, "they": {}, "he": {}, "she": {},
	"it": {}, "this": {}, "that": {}, "these": {}, "those": {}, "what": {},
	"which": {}, "who": {}, "whom": {}, "whose": {}, "when": {}, "where": {},
	"why": {}, "how": {}, "my": {}, "your": {}, "our": {}, "their": {},
	"his": {}, "her": {}, "its": {}, "me": {}, "us": {}, "them": {}, "him": {},
	"all": {}, "any": {}, "some": {}, "no": {}, "not": {}, "only": {}, "also": {},
	"so": {}, "than": {}, "then": {}, "there": {}, "here": {}, "just": {},
	"like": {}, "very": {}, "much": {}, "many": {}, "more": {}, "most": {},
	"few": {}, "less": {}, "out": {}, "up": {}, "down": {}, "off": {},
	"again": {}, "now": {}, "ever": {}, "still": {},
}

// contentWords extracts lowercase content tokens from `query`. Splits on
// non-letter/digit, drops length<3 and stopwords. Used by the keyword-
// overlap channel — a query of "What database did we choose?" yields
// ["database", "choose"]; "where do we deploy our services?" yields
// ["deploy", "services"].
func contentWords(query string) []string {
	if query == "" {
		return nil
	}
	low := strings.ToLower(query)
	var b strings.Builder
	tokens := make([]string, 0, 16)
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := b.String()
		b.Reset()
		if len(w) < 3 {
			return
		}
		if _, ok := stopwords[w]; ok {
			return
		}
		tokens = append(tokens, w)
	}
	for i := 0; i < len(low); i++ {
		c := low[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			flush()
		}
	}
	flush()
	// De-dupe, preserving order.
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
