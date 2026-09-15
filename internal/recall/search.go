package recall

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/vec"
)

// Search modes for hybrid recall.
const (
	ModeHybrid   = "hybrid"   // RRF over both lexical + semantic; default
	ModeLexical  = "lexical"  // FTS5 only, returns turns
	ModeSemantic = "semantic" // cosine only, returns decisions (HyDE-expanded query if enabled)
)

// Embedder is the minimal interface Search needs. Pass any concrete
// embedder (apple, onnx) — they satisfy this interface. Nil is allowed
// and forces lexical-only behaviour.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Name() string
}

type SearchOptions struct {
	Query    string
	Project  string // ancestor-scoped; empty = all projects
	Limit    int
	Mode     string // ModeHybrid (default), ModeLexical, ModeSemantic
	Embedder Embedder
	UseHyDE  bool   // when true, ask Haiku for a hypothetical answer before embedding
	Entity   string // case-insensitive entity name; empty = no filter. Restricts
	// FTS + cosine candidate pools to rows that mention @<Entity>. Resolves
	// to an entities.id internally; missing entities short-circuit to no hits.
}

// SearchHit is a unified result type spanning both turn matches (FTS)
// and decision matches (cosine). Inspect Kind to demux.
type SearchHit struct {
	Kind         string   `json:"kind"` // "turn" | "decision"
	SessionID    string   `json:"session_id,omitempty"`
	Role         string   `json:"role,omitempty"`
	Ts           string   `json:"ts,omitempty"`
	Excerpt      string   `json:"excerpt,omitempty"`
	DecisionID   int64    `json:"decision_id,omitempty"`
	DecisionKind string   `json:"decision_kind,omitempty"`
	Text         string   `json:"text,omitempty"`
	ProjectDir   string   `json:"project_dir,omitempty"`
	Score        float64  `json:"score"`
	Channels     []string `json:"channels"`
}

type SearchResult struct {
	Hits          []SearchHit `json:"hits"`
	Count         int         `json:"count"`
	Mode          string      `json:"mode"`
	Query         string      `json:"query"`
	ExpandedQuery string      `json:"expanded_query,omitempty"`
	EmbedderName  string      `json:"embedder,omitempty"`
}

// Search runs lexical (FTS5 over turn_fts) and / or semantic (cosine over
// decisions.embedding) channels and fuses results via RRF when mode is
// hybrid. The semantic channel optionally prepends a HyDE expansion when
// UseHyDE is true and an Anthropic key is configured. Falls back to
// lexical-only when no embedder is provided.
func Search(ctx context.Context, db *sql.DB, opts SearchOptions) (*SearchResult, error) {
	if opts.Mode == "" {
		opts.Mode = ModeHybrid
	}
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	if opts.Embedder == nil && (opts.Mode == ModeSemantic || opts.Mode == ModeHybrid) {
		// Degrade to lexical when semantic is requested but unavailable.
		opts.Mode = ModeLexical
	}

	res := &SearchResult{Query: opts.Query, Mode: opts.Mode}
	if opts.Embedder != nil {
		res.EmbedderName = opts.Embedder.Name()
	}

	var (
		lexCh       Channel
		semCh       Channel
		turnDetails map[string]turnDetail
		decDetails  map[int64]decisionDetail
	)

	runLex := opts.Mode == ModeLexical || opts.Mode == ModeHybrid
	runSem := (opts.Mode == ModeSemantic || opts.Mode == ModeHybrid) && opts.Embedder != nil

	// Each channel pulls limit*3 to give RRF enough overlap candidates;
	// the final list is truncated to opts.Limit at the end.
	pool := opts.Limit * 3
	if pool < 10 {
		pool = 10
	}

	// Resolve --entity to an entities.id once. If the user passed an entity
	// name we don't have any mentions for, short-circuit: every channel
	// returns empty rather than running a query that could never match.
	var entityID int64
	if opts.Entity != "" {
		var err error
		entityID, err = lookupEntityID(ctx, db, opts.Entity)
		if err != nil {
			return nil, fmt.Errorf("entity lookup: %w", err)
		}
		if entityID == 0 {
			return &SearchResult{Query: opts.Query, Mode: opts.Mode, EmbedderName: res.EmbedderName, Hits: []SearchHit{}, Count: 0}, nil
		}
	}

	if runLex {
		ch, td, err := ftsTurnsChannel(ctx, db, opts.Query, opts.Project, pool, entityID)
		if err != nil {
			return nil, fmt.Errorf("lexical fts: %w", err)
		}
		lexCh = ch
		turnDetails = td
	}

	if runSem {
		embedQuery := opts.Query
		if opts.UseHyDE {
			expanded := Expand(ctx, opts.Query)
			if expanded != "" && expanded != opts.Query {
				embedQuery = expanded
				res.ExpandedQuery = expanded
			}
		}
		qv, err := opts.Embedder.Embed(ctx, embedQuery)
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		ch, dd, err := cosineDecisionsChannel(ctx, db, qv, opts.Project, pool, entityID)
		if err != nil {
			return nil, fmt.Errorf("semantic cosine: %w", err)
		}
		semCh = ch
		decDetails = dd
	}

	// In hybrid mode, also build a temporal channel: re-rank the union of
	// lexical+semantic candidates by recency. RRF with three channels means
	// a hit that's lexically relevant AND recent outranks one that's only
	// lexically relevant. Doesn't introduce new candidates — it only
	// re-orders the ones lex/sem already surfaced.
	var tempCh Channel
	if opts.Mode == ModeHybrid {
		ids := unionChannelIDs(lexCh, semCh)
		if len(ids) > 0 {
			ch, err := temporalChannel(ctx, db, ids)
			if err != nil {
				return nil, fmt.Errorf("temporal: %w", err)
			}
			tempCh = ch
		}
	}

	// Fuse / select
	var fused []Hit
	switch opts.Mode {
	case ModeLexical:
		// channel index 0 → "lexical" in the channelNames map below
		fused = singleChannelToFusedHits(lexCh, opts.Limit, 0)
	case ModeSemantic:
		// channel index 1 → "semantic"
		fused = singleChannelToFusedHits(semCh, opts.Limit, 1)
	default: // hybrid
		channels := []Channel{lexCh, semCh, tempCh}
		fused = Fuse(channels, DefaultK, opts.Limit)
	}

	channelNames := []string{"lexical", "semantic", "temporal"}
	hits := make([]SearchHit, 0, len(fused))
	for _, h := range fused {
		sh := SearchHit{Score: h.Score}
		for _, ci := range h.Channels {
			if ci >= 0 && ci < len(channelNames) {
				sh.Channels = append(sh.Channels, channelNames[ci])
			}
		}
		switch {
		case strings.HasPrefix(h.ID, "turn:"):
			sh.Kind = "turn"
			d, ok := turnDetails[h.ID]
			if ok {
				sh.SessionID = d.sessionID
				sh.Role = d.role
				sh.Ts = d.ts
				sh.Excerpt = d.excerpt
				sh.ProjectDir = d.projectDir
			}
		case strings.HasPrefix(h.ID, "dec:"):
			sh.Kind = "decision"
			var did int64
			fmt.Sscanf(h.ID, "dec:%d", &did)
			d, ok := decDetails[did]
			if ok {
				sh.DecisionID = d.id
				sh.DecisionKind = d.kind
				sh.Text = d.text
				sh.ProjectDir = d.projectDir
				if d.ts > 0 {
					sh.Ts = time.Unix(d.ts, 0).UTC().Format(time.RFC3339)
				}
			}
		}
		hits = append(hits, sh)
	}
	res.Hits = hits
	res.Count = len(hits)
	return res, nil
}

func singleChannelToFusedHits(ch Channel, limit, channelIdx int) []Hit {
	out := make([]Hit, 0, len(ch))
	for _, h := range ch {
		out = append(out, Hit{ID: h.ID, Score: h.Score, Channels: []int{channelIdx}})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

type turnDetail struct {
	sessionID  string
	role       string
	ts         string
	excerpt    string
	projectDir string
}

// sanitizeFTS5 turns a natural-language query into a safe FTS5 MATCH
// expression. Tokens that are pure alphanumerics (with an optional trailing
// `*` for prefix search) pass through unquoted so AND-of-terms semantics work
// as users expect. Tokens containing any FTS5 meta character (`-`, `:`, `(`,
// `)`, `^`, `"`, `*` mid-token, etc.) get wrapped in double quotes so they're
// treated as literal phrases — this is what blew up Codex's first call when
// the bare-word query `recall project architecture …` parsed as
// `agent NOT memory` and FTS5 reported `no such column: memory`.
//
// Empty input returns "" — caller should treat as "no lexical channel".
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
		// Phrase-quote: escape internal double quotes by doubling, per FTS5 syntax.
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
	if s[end-1] == '*' { // allow trailing prefix-search marker
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

func ftsTurnsChannel(ctx context.Context, db *sql.DB, query, project string, limit int, entityID int64) (Channel, map[string]turnDetail, error) {
	safe := sanitizeFTS5(query)
	if safe == "" {
		return nil, map[string]turnDetail{}, nil
	}
	args := []any{safe}
	where := "turn_fts MATCH ?"
	if project != "" {
		where += " AND (sessions.project_dir = ? OR ? LIKE (sessions.project_dir || '/%'))"
		args = append(args, project, project)
	}
	if entityID > 0 {
		where += " AND turns.uuid IN (SELECT source_id FROM entity_mentions WHERE entity_id = ? AND source_kind = 'turn')"
		args = append(args, entityID)
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
		SELECT turns.uuid, turns.role, COALESCE(turns.ts, 0),
		       COALESCE(sessions.id, ''), COALESCE(sessions.project_dir, ''),
		       snippet(turn_fts, 0, '<<', '>>', '...', 24) AS excerpt
		  FROM turn_fts
		  JOIN turns    ON turn_fts.rowid = turns.rowid
		  JOIN sessions ON turns.session_id = sessions.id
		 WHERE %s
		 ORDER BY rank, turns.ts DESC
		 LIMIT ?`, where)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var ch Channel
	details := map[string]turnDetail{}
	rank := 0
	for rows.Next() {
		var uuid, role, sessionID, projectDir, excerpt string
		var ts int64
		if err := rows.Scan(&uuid, &role, &ts, &sessionID, &projectDir, &excerpt); err != nil {
			return nil, nil, err
		}
		id := "turn:" + uuid
		ch = append(ch, Hit{ID: id, Score: 1.0 / float64(rank+1)})
		d := turnDetail{sessionID: sessionID, role: role, projectDir: projectDir, excerpt: excerpt}
		if ts > 0 {
			d.ts = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		}
		details[id] = d
		rank++
	}
	return ch, details, rows.Err()
}

type decisionDetail struct {
	id         int64
	kind       string
	text       string
	projectDir string
	ts         int64
}

// cosineDecisionsChannel scores decisions against the query vector,
// taking the **max cosine** between the query and the decision's
// canonical embedding OR any of its paraphrase embeddings (the
// `decision_paraphrases` table populated by `recall paraphrase`).
// Falls back gracefully when paraphrases haven't been generated.
func cosineDecisionsChannel(ctx context.Context, db *sql.DB, queryVec []float32, project string, limit int, entityID int64) (Channel, map[int64]decisionDetail, error) {
	// First load decisions with canonical embeddings.
	where := "embedding IS NOT NULL AND superseded_by IS NULL"
	var args []any
	if project != "" {
		where += " AND (project_dir = ? OR project_dir IS NULL OR ? LIKE (project_dir || '/%'))"
		args = append(args, project, project)
	}
	if entityID > 0 {
		// source_id is stored as TEXT; SQLite implicit conversion compares cleanly to id.
		where += " AND id IN (SELECT CAST(source_id AS INTEGER) FROM entity_mentions WHERE entity_id = ? AND source_kind = 'decision')"
		args = append(args, entityID)
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, kind, text, COALESCE(project_dir, ''), COALESCE(ts, 0), embedding
		             FROM decisions WHERE %s`, where), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	bestScore := map[int64]float32{}
	details := map[int64]decisionDetail{}
	for rows.Next() {
		var d decisionDetail
		var blob []byte
		if err := rows.Scan(&d.id, &d.kind, &d.text, &d.projectDir, &d.ts, &blob); err != nil {
			return nil, nil, err
		}
		v := vec.Decode(blob)
		if len(v) == 0 {
			continue
		}
		score := vec.Cosine(queryVec, v)
		bestScore[d.id] = score
		details[d.id] = d
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Now overlay paraphrase embeddings: take the max per decision_id.
	// If a paraphrase has higher cosine than the canonical, it wins
	// (this is exactly what we want — paraphrases are alternate
	// phrasings the user might query for, not noise).
	if len(details) > 0 {
		paraphraseWhere := "decision_paraphrases.embedding IS NOT NULL AND decisions.superseded_by IS NULL"
		paraphraseArgs := []any{}
		if project != "" {
			paraphraseWhere += " AND (decisions.project_dir = ? OR decisions.project_dir IS NULL OR ? LIKE (decisions.project_dir || '/%'))"
			paraphraseArgs = append(paraphraseArgs, project, project)
		}
		pRows, err := db.QueryContext(ctx, fmt.Sprintf(`
			SELECT decisions.id, decision_paraphrases.embedding
			  FROM decision_paraphrases
			  JOIN decisions ON decision_paraphrases.decision_id = decisions.id
			 WHERE %s`, paraphraseWhere), paraphraseArgs...)
		if err == nil {
			defer pRows.Close()
			for pRows.Next() {
				var did int64
				var blob []byte
				if err := pRows.Scan(&did, &blob); err != nil {
					continue
				}
				v := vec.Decode(blob)
				if len(v) == 0 {
					continue
				}
				score := vec.Cosine(queryVec, v)
				if existing, ok := bestScore[did]; !ok || score > existing {
					bestScore[did] = score
				}
			}
		}
	}

	// Sort by score, take top-K
	type scored struct {
		id    int64
		score float32
	}
	out := make([]scored, 0, len(bestScore))
	for id, s := range bestScore {
		out = append(out, scored{id: id, score: s})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	ch := make(Channel, 0, len(out))
	finalDetails := map[int64]decisionDetail{}
	for _, s := range out {
		id := fmt.Sprintf("dec:%d", s.id)
		ch = append(ch, Hit{ID: id, Score: float64(s.score)})
		if d, ok := details[s.id]; ok {
			finalDetails[s.id] = d
		}
	}
	return ch, finalDetails, nil
}

// unionChannelIDs returns the de-duplicated set of IDs across the supplied
// channels, preserving first-appearance order so the temporal re-rank is
// stable when timestamps tie.
// lookupEntityID resolves a (case-insensitive) entity name to its row id.
// Returns 0 if the name has never been mentioned — search treats that as
// "no candidates" and short-circuits.
func lookupEntityID(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM entities WHERE name = ?`,
		strings.ToLower(strings.TrimSpace(name))).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

func unionChannelIDs(channels ...Channel) []string {
	seen := map[string]bool{}
	ids := make([]string, 0)
	for _, ch := range channels {
		for _, h := range ch {
			if seen[h.ID] {
				continue
			}
			seen[h.ID] = true
			ids = append(ids, h.ID)
		}
	}
	return ids
}

// temporalChannel re-ranks the supplied IDs by recency. Looks up turn.ts
// for "turn:<uuid>" and decision.ts for "dec:<id>"; sorts descending so
// the most-recent items lead. IDs whose timestamp can't be resolved fall
// to the end with ts=0 (still valid candidates, just no recency boost).
//
// Why this is its own RRF channel rather than a multiplicative boost:
// RRF is rank-based and works well when each channel orders the same
// candidate set by a different criterion. Treating recency as the third
// criterion keeps the math symmetric with FTS5 + cosine and gracefully
// degrades when ts is missing (those items just sink to the bottom of
// this channel without breaking the others).
func temporalChannel(ctx context.Context, db *sql.DB, ids []string) (Channel, error) {
	turnTs := map[string]int64{}
	decTs := map[string]int64{}

	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "turn:"):
			turnTs[strings.TrimPrefix(id, "turn:")] = 0
		case strings.HasPrefix(id, "dec:"):
			decTs[strings.TrimPrefix(id, "dec:")] = 0
		}
	}

	if len(turnTs) > 0 {
		uuids := make([]any, 0, len(turnTs))
		for u := range turnTs {
			uuids = append(uuids, u)
		}
		placeholders := strings.Repeat("?,", len(uuids))
		placeholders = placeholders[:len(placeholders)-1]
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf(`SELECT uuid, COALESCE(ts, 0) FROM turns WHERE uuid IN (%s)`, placeholders),
			uuids...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var uuid string
			var ts int64
			if err := rows.Scan(&uuid, &ts); err != nil {
				rows.Close()
				return nil, err
			}
			turnTs[uuid] = ts
		}
		rows.Close()
	}

	if len(decTs) > 0 {
		dids := make([]any, 0, len(decTs))
		for d := range decTs {
			dids = append(dids, d)
		}
		placeholders := strings.Repeat("?,", len(dids))
		placeholders = placeholders[:len(placeholders)-1]
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, COALESCE(ts, 0) FROM decisions WHERE id IN (%s)`, placeholders),
			dids...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var did string
			var ts int64
			if err := rows.Scan(&did, &ts); err != nil {
				rows.Close()
				return nil, err
			}
			decTs[did] = ts
		}
		rows.Close()
	}

	// Build (id, ts) pairs in input order, then stable-sort by ts desc.
	type tsHit struct {
		id string
		ts int64
	}
	hits := make([]tsHit, 0, len(ids))
	for _, id := range ids {
		var ts int64
		switch {
		case strings.HasPrefix(id, "turn:"):
			ts = turnTs[strings.TrimPrefix(id, "turn:")]
		case strings.HasPrefix(id, "dec:"):
			ts = decTs[strings.TrimPrefix(id, "dec:")]
		}
		hits = append(hits, tsHit{id: id, ts: ts})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].ts > hits[j].ts })

	ch := make(Channel, 0, len(hits))
	for _, h := range hits {
		ch = append(ch, Hit{ID: h.id, Score: float64(h.ts)})
	}
	return ch, nil
}
