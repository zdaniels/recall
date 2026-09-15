// Package inject builds the project-aware context block that Claude Code's
// SessionStart hook prepends. Stays under a token budget; emits nothing for
// projects we have no record of (avoids polluting fresh sessions).
package inject

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/store"
)

// groundTruthDirective is the "anti-amnesia" preamble rendered above the
// high-authority sections (pins / instructions / decisions). It turns the
// soft CLAUDE.md nudge ("call recall_search before saying you don't know")
// into an explicit, in-band instruction that travels with the memory block
// itself — so Codex / Cursor (which have no SessionStart hook and only see
// this via recall_summary) get the same framing. We emit it only when there
// is actually authoritative content to frame; a files-only block doesn't
// warrant it.
const groundTruthDirective = "Ground truth — treat the items below as established for this project. " +
	"Prefer them over re-deriving or re-asking; if you must contradict one, say so explicitly and call record_decision to update it.\n"

type Options struct {
	Project          string
	SessionID        string // if set, include "This session so far" section
	Query            string // optional — FTS5 query to surface relevant prior turns
	Days             int    // lookback window
	Budget           int    // approx tokens (1 token ≈ 4 chars)
	MaxFiles         int
	MaxSessions      int
	MaxDecisions     int
	MaxQueryHits     int
	MaxRelated       int // spreading-activation neighbors via edges
	MaxPins          int
	MaxSessionFiles  int // files from this session (when SessionID set)
	MaxSessionEvents int // surprise events from this session
	MaxTopics        int // recent user-turn excerpts surfaced as "topics"
}

func (o *Options) defaults() {
	if o.Days == 0 {
		o.Days = 30
	}
	if o.Budget == 0 {
		o.Budget = 250
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = 8
	}
	if o.MaxSessions == 0 {
		o.MaxSessions = 5
	}
	if o.MaxDecisions == 0 {
		o.MaxDecisions = 6
	}
	if o.MaxQueryHits == 0 {
		o.MaxQueryHits = 3
	}
	if o.MaxRelated == 0 {
		o.MaxRelated = 4
	}
	if o.MaxPins == 0 {
		o.MaxPins = 10
	}
	if o.MaxSessionFiles == 0 {
		o.MaxSessionFiles = 8
	}
	if o.MaxSessionEvents == 0 {
		o.MaxSessionEvents = 4
	}
	if o.MaxTopics == 0 {
		o.MaxTopics = 4
	}
}

// Render writes the context block to w. Returns (wroteSomething, err).
// When the project has no recorded activity, returns (false, nil) and writes nothing.
func Render(ctx context.Context, st *store.Store, opts Options, w io.Writer) (bool, error) {
	opts.defaults()
	if opts.Project == "" {
		return false, nil
	}
	cutoff := time.Now().AddDate(0, 0, -opts.Days).Unix()

	pins, _ := st.ActivePins(ctx, opts.SessionID, opts.Project, opts.MaxPins)
	// Instructions get their own quota — they're procedural runbook content
	// that's always actionable, so we don't want them outranked by chatty
	// older decisions with higher use_count.
	instructions, err := relevantDecisionsByKind(ctx, st.DB(), opts.Project, "instruction", 6)
	if err != nil {
		return false, fmt.Errorf("instructions: %w", err)
	}
	decisions, err := relevantDecisionsExcludingKinds(ctx, st.DB(), opts.Project, []string{"instruction"}, opts.MaxDecisions)
	if err != nil {
		return false, fmt.Errorf("decisions: %w", err)
	}
	var (
		sessFiles  []fileRow
		sessEvents []sessionEvent
	)
	if opts.SessionID != "" {
		sessFiles, _ = sessionFiles(ctx, st.DB(), opts.SessionID, opts.MaxSessionFiles)
		sessEvents, _ = sessionEvents(ctx, st.DB(), opts.SessionID, opts.MaxSessionEvents)
	}
	sessions, err := recentSessions(ctx, st.DB(), opts.Project, cutoff, opts.MaxSessions)
	if err != nil {
		return false, fmt.Errorf("sessions: %w", err)
	}
	files, err := recentFiles(ctx, st.DB(), opts.Project, cutoff, opts.MaxFiles)
	if err != nil {
		return false, fmt.Errorf("files: %w", err)
	}
	// Recent user-turn excerpts. These are topic-launchers from prior
	// sessions — usually questions or task descriptions like "what's the
	// runner config" or "deploy xcs-web-app". Surfacing them in the inject
	// means the agent sees recent project context without having to call
	// recall_search proactively, which closes the gap between "data is
	// captured" and "agent uses captured data".
	topics, _ := recentTopics(ctx, st.DB(), opts.Project, cutoff, opts.MaxTopics)
	// Spreading activation: pull neighbors of the recent files via the
	// file<->file edges populated during ingest. Excludes anything
	// already in `files` so we don't double-count.
	var related []relatedFile
	if len(files) > 0 {
		seeds := make([]string, len(files))
		seen := make(map[string]struct{}, len(files))
		for i, f := range files {
			seeds[i] = f.path
			seen[f.path] = struct{}{}
		}
		related, _ = relatedFiles(ctx, st.DB(), seeds, seen, opts.MaxRelated)
	}
	var hits []queryHit
	if q := strings.TrimSpace(opts.Query); q != "" {
		hits, err = queryHits(ctx, st.DB(), opts.Project, q, opts.MaxQueryHits)
		if err != nil {
			// FTS5 syntax errors shouldn't break the hook — degrade to no hits.
			hits = nil
		}
	}

	if len(pins) == 0 && len(decisions) == 0 && len(sessions) == 0 && len(files) == 0 &&
		len(hits) == 0 && len(related) == 0 && len(sessFiles) == 0 && len(sessEvents) == 0 &&
		len(topics) == 0 {
		return false, nil
	}

	// Bump use_count for surfaced decisions so frequently-recalled memories
	// rise in ranking. Best-effort — never fails the inject path.
	if len(decisions) > 0 {
		ids := make([]int64, len(decisions))
		for i, d := range decisions {
			ids[i] = d.id
		}
		st.BumpUseCount(ctx, ids)
	}

	var sb strings.Builder
	sb.WriteString("<recall>\n")
	sb.WriteString(fmt.Sprintf("recall for %s (last %dd):\n", opts.Project, opts.Days))
	// Ground-truth preamble: only when we have high-authority content to
	// frame (pins / instructions / decisions). Skip it for blocks that are
	// only recent-files / session breadcrumbs — there's nothing to assert.
	if len(pins) > 0 || len(instructions) > 0 || len(decisions) > 0 {
		sb.WriteString(groundTruthDirective)
	}
	// Pinned items first — explicit "remember this" markers for the session
	// or project. Surface always, regardless of budget pressure.
	if len(pins) > 0 {
		sb.WriteString("📌 Pinned (cleared with `recall unpin <id>`):\n")
		for _, p := range pins {
			sb.WriteString(fmt.Sprintf("  - #%d %s\n", p.ID, p.Text))
		}
	}
	if len(instructions) > 0 {
		sb.WriteString("Instructions (procedural; follow these when relevant):\n")
		for _, d := range instructions {
			sb.WriteString(fmt.Sprintf("  - [#%d]%s %s\n", d.id, confidenceMark(d.confidence), d.text))
		}
	}
	if len(decisions) > 0 {
		sb.WriteString("Decisions / preferences (refer by id with recall_decisions; ✓ = corroborated):\n")
		for _, d := range decisions {
			sb.WriteString(fmt.Sprintf("  - [%s #%d]%s %s\n", d.kind, d.id, confidenceMark(d.confidence), d.text))
		}
	}
	// "This session so far" — present only when we know the session id.
	// Bridges context across compaction / clearing within a long session.
	if len(sessFiles) > 0 || len(sessEvents) > 0 {
		sb.WriteString("This session so far:\n")
		if len(sessFiles) > 0 {
			sb.WriteString("  files touched:\n")
			for _, f := range sessFiles {
				sb.WriteString(fmt.Sprintf("    - %s (×%d)\n", f.path, f.count))
			}
		}
		if len(sessEvents) > 0 {
			sb.WriteString("  notable events:\n")
			for _, e := range sessEvents {
				sb.WriteString(fmt.Sprintf("    - [%s] %s\n", e.kind, e.text))
			}
		}
	}
	if len(topics) > 0 {
		sb.WriteString("Recent topics (user-turn excerpts; if you suspect prior context, prefer recall_search over asking):\n")
		for _, t := range topics {
			day := time.Unix(t.ts, 0).Format("01-02")
			sb.WriteString(fmt.Sprintf("  - %s — %s\n", day, t.excerpt))
		}
	}
	if len(sessions) > 0 {
		sb.WriteString("Recent sessions:\n")
		for _, s := range sessions {
			day := time.Unix(s.endedAt, 0).Format("01-02")
			sb.WriteString(fmt.Sprintf("  - %s — %s\n", day, s.summary))
		}
	}
	if len(files) > 0 {
		sb.WriteString("Recently touched files:\n")
		for _, f := range files {
			sb.WriteString(fmt.Sprintf("  - %s (×%d)\n", f.path, f.count))
		}
	}
	if len(related) > 0 {
		sb.WriteString("Often touched alongside (co-occurrence graph):\n")
		for _, r := range related {
			sb.WriteString(fmt.Sprintf("  - %s (link weight %.0f)\n", r.path, r.weight))
		}
	}
	if len(hits) > 0 {
		sb.WriteString("Relevant prior turns (matching this prompt):\n")
		for _, h := range hits {
			sb.WriteString(fmt.Sprintf("  - %s: %s\n", h.role, h.excerpt))
		}
	}
	sb.WriteString("</recall>\n")

	out := sb.String()
	maxChars := opts.Budget * 4
	if len(out) > maxChars {
		out = out[:maxChars] + "\n…</recall>\n"
	}
	if _, err := io.WriteString(w, out); err != nil {
		return true, err
	}
	return true, nil
}

type decisionRow struct {
	id         int64
	kind       string
	text       string
	confidence float64
}

// confirmedConfidence is the threshold at or above which a decision is shown
// as corroborated ("✓") in the inject block. The default confidence is 0.5,
// so only facts that have actually been re-asserted (or otherwise confirmed)
// cross it — neutral and unverified rows render unadorned.
const confirmedConfidence = 0.7

// relevantDecisionsByKind is like relevantDecisions but filtered to one kind.
func relevantDecisionsByKind(ctx context.Context, db *sql.DB, project, kind string, limit int) ([]decisionRow, error) {
	q := `SELECT id, kind, text, COALESCE(confidence, 0.5)
		  FROM decisions
		 WHERE superseded_by IS NULL
		   AND kind = ?
		   AND (
		     project_dir = ?
		     OR project_dir IS NULL
		     OR ? LIKE (project_dir || '/%')
		   )
		 ORDER BY
		   CASE source WHEN 'pattern' THEN 1 ELSE 0 END,
		   ` + store.EffectiveSalienceExpr + ` DESC,
		   COALESCE(ts, 0) DESC
		 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, kind, project, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []decisionRow
	for rows.Next() {
		var d decisionRow
		if err := rows.Scan(&d.id, &d.kind, &d.text, &d.confidence); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// relevantDecisionsExcludingKinds returns top decisions excluding the
// given kinds (typically used to skip 'instruction' since it has its own
// dedicated section).
func relevantDecisionsExcludingKinds(ctx context.Context, db *sql.DB, project string, excludeKinds []string, limit int) ([]decisionRow, error) {
	if len(excludeKinds) == 0 {
		return relevantDecisions(ctx, db, project, limit)
	}
	placeholders := strings.Repeat("?,", len(excludeKinds))
	placeholders = placeholders[:len(placeholders)-1]
	args := []any{project, project}
	for _, k := range excludeKinds {
		args = append(args, k)
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT id, kind, text, COALESCE(confidence, 0.5)
		  FROM decisions
		 WHERE superseded_by IS NULL
		   AND kind NOT IN (%s)
		   AND (
		     project_dir = ?
		     OR project_dir IS NULL
		     OR ? LIKE (project_dir || '/%%')
		   )
		 ORDER BY
		   CASE source WHEN 'pattern' THEN 1 ELSE 0 END,
		   `+store.EffectiveSalienceExpr+` DESC,
		   COALESCE(ts, 0) DESC
		 LIMIT ?`, placeholders)
	// Reorder args to match the placeholder positions: kinds first, then project, project, limit.
	finalArgs := []any{}
	for _, k := range excludeKinds {
		finalArgs = append(finalArgs, k)
	}
	finalArgs = append(finalArgs, project, project, limit)
	rows, err := db.QueryContext(ctx, q, finalArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []decisionRow
	for rows.Next() {
		var d decisionRow
		if err := rows.Scan(&d.id, &d.kind, &d.text, &d.confidence); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// relevantDecisions returns active decisions for the project, including ancestors
// (e.g. /Users/z decisions surface for /Users/z/Documents/project). Sources
// other than 'pattern' are ranked first since they're confirmed; within a source
// ordering is by effective salience (base × time decay + use boost). Globally-
// scoped (NULL project_dir) decisions are also included.
func relevantDecisions(ctx context.Context, db *sql.DB, project string, limit int) ([]decisionRow, error) {
	q := `SELECT id, kind, text, COALESCE(confidence, 0.5)
		  FROM decisions
		 WHERE superseded_by IS NULL
		   AND (
		     project_dir = ?
		     OR project_dir IS NULL
		     OR ? LIKE (project_dir || '/%')
		   )
		 ORDER BY
		   CASE source WHEN 'pattern' THEN 1 ELSE 0 END,  -- confirmed sources first
		   ` + store.EffectiveSalienceExpr + ` DESC,
		   COALESCE(ts, 0) DESC
		 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, project, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []decisionRow
	for rows.Next() {
		var d decisionRow
		if err := rows.Scan(&d.id, &d.kind, &d.text, &d.confidence); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type sessionRow struct {
	endedAt int64
	summary string
}

func recentSessions(ctx context.Context, db *sql.DB, project string, cutoff int64, limit int) ([]sessionRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(ended_at, started_at, 0), COALESCE(summary, '')
		   FROM sessions
		  WHERE project_dir = ?
		    AND COALESCE(ended_at, started_at, 0) >= ?
		    AND summary IS NOT NULL AND summary != ''
		  ORDER BY ended_at DESC LIMIT ?`,
		project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.endedAt, &s.summary); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type fileRow struct {
	path  string
	count int
}

type topicRow struct {
	ts      int64
	excerpt string
}

// recentTopics returns user-turn excerpts from prior sessions in this project,
// recent first. Filters to length-substantive turns (between MinTopicLen and
// MaxTopicLen chars) so we get meaningful question/task launchers, not "ok"
// or pasted code blocks. De-duplicates by exact excerpt to avoid noisy
// repeats when the user rephrases the same prompt across sessions.
//
// The agent uses these as breadcrumbs into past discussions — when it sees
// "what's the github runner config?" in this section it has a strong signal
// that running recall_search on the topic will find the answer.
const (
	minTopicLen = 60
	maxTopicLen = 400
	excerptCap  = 140
)

func recentTopics(ctx context.Context, db *sql.DB, project string, cutoff int64, limit int) ([]topicRow, error) {
	if limit <= 0 || project == "" {
		return nil, nil
	}
	// Pull up to 4× the limit so we can dedupe + filter junk and still hit limit.
	pool := limit * 4
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(turns.ts, 0), turns.text
		  FROM turns
		  JOIN sessions ON sessions.id = turns.session_id
		 WHERE sessions.project_dir = ?
		   AND COALESCE(turns.ts, 0) >= ?
		   AND turns.role = 'user'
		   AND length(turns.text) BETWEEN ? AND ?
		 ORDER BY turns.ts DESC
		 LIMIT ?
	`, project, cutoff, minTopicLen, maxTopicLen, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	var out []topicRow
	for rows.Next() {
		var ts int64
		var text string
		if err := rows.Scan(&ts, &text); err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		// Collapse internal whitespace so multi-line prompts render compact.
		text = whitespaceRE.ReplaceAllString(text, " ")
		// Skip system-generated turns (task notifications, hook outputs)
		// that have no value as user-driven topic launchers.
		if strings.HasPrefix(text, "<task-notification>") ||
			strings.HasPrefix(text, "<system-reminder>") ||
			strings.HasPrefix(text, "<command-name>") {
			continue
		}
		// Skip turns that look like pasted output rather than user-typed
		// prompts (lots of brackets / pipes / hashes). Heuristic; not perfect.
		if punctRatio(text) > 0.18 {
			continue
		}
		excerpt := text
		if len(excerpt) > excerptCap {
			excerpt = excerpt[:excerptCap] + "…"
		}
		// Dedup by the first ~80 chars normalized so "X" and "X plus more
		// text" don't both surface as separate topics.
		dedupKey := strings.ToLower(excerpt)
		if len(dedupKey) > 80 {
			dedupKey = dedupKey[:80]
		}
		if _, ok := seen[dedupKey]; ok {
			continue
		}
		seen[dedupKey] = struct{}{}
		out = append(out, topicRow{ts: ts, excerpt: excerpt})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// confidenceMark returns a compact corroboration marker for a decision's
// confidence: " ✓" once it crosses confirmedConfidence, empty otherwise.
// Kept tiny so it doesn't eat the inject token budget.
func confidenceMark(confidence float64) string {
	if confidence >= confirmedConfidence {
		return " ✓"
	}
	return ""
}

var whitespaceRE = regexp.MustCompile(`\s+`)

// punctRatio is the fraction of non-alphanumeric, non-space chars in s.
// User-typed prompts hover around 0.10; pasted command output (kubectl,
// JSON, log lines) is usually >0.25.
func punctRatio(s string) float64 {
	if s == "" {
		return 0
	}
	punct := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' {
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c >= 0x80 {
			continue
		}
		punct++
	}
	return float64(punct) / float64(len(s))
}

type queryHit struct {
	role    string
	excerpt string
}

type sessionEvent struct {
	kind string
	text string
}

func sessionFiles(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]fileRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT path, COUNT(*) AS n
		   FROM files
		  WHERE session_id = ?
		  GROUP BY path
		  ORDER BY MAX(COALESCE(ts, 0)) DESC, n DESC
		  LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.path, &f.count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// sessionEvents surfaces decisions captured during the active session —
// surprise rows (bash failures, edit-reverts) and pattern decisions stick.
// The query is scoped on session_id so bridges context across compaction.
func sessionEvents(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]sessionEvent, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT kind, text
		   FROM decisions
		  WHERE session_id = ? AND superseded_by IS NULL
		    AND source IN ('surprise', 'pattern', 'tool')
		  ORDER BY ts DESC
		  LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionEvent
	for rows.Next() {
		var e sessionEvent
		if err := rows.Scan(&e.kind, &e.text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type relatedFile struct {
	path   string
	weight float64
}

// relatedFiles spreads activation across the file↔file co-occurrence graph
// from the seed files. For each seed we find its strongest neighbors,
// dedup, exclude anything in the seed set, and return the top-K by
// aggregated weight. Cheap (a single SQL query, a Go-side merge).
func relatedFiles(ctx context.Context, db *sql.DB, seeds []string, exclude map[string]struct{}, k int) ([]relatedFile, error) {
	if len(seeds) == 0 || k <= 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(seeds))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(seeds)*2)
	for _, s := range seeds {
		args = append(args, s)
	}
	for _, s := range seeds {
		args = append(args, s)
	}

	q := fmt.Sprintf(`
		SELECT neighbor, SUM(weight) AS total
		  FROM (
		    -- src is a seed, dst is the neighbor
		    SELECT dst_id AS neighbor, weight
		      FROM edges
		     WHERE src_kind = 'file' AND dst_kind = 'file'
		       AND src_id IN (%s)
		    UNION ALL
		    -- dst is a seed, src is the neighbor (edges are stored canonically)
		    SELECT src_id AS neighbor, weight
		      FROM edges
		     WHERE src_kind = 'file' AND dst_kind = 'file'
		       AND dst_id IN (%s)
		  )
		 GROUP BY neighbor
		 ORDER BY total DESC`, placeholders, placeholders)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []relatedFile
	for rows.Next() {
		var path string
		var total float64
		if err := rows.Scan(&path, &total); err != nil {
			return nil, err
		}
		if _, skip := exclude[path]; skip {
			continue
		}
		out = append(out, relatedFile{path: path, weight: total})
		if len(out) >= k {
			break
		}
	}
	return out, rows.Err()
}

// queryHits runs the user's prompt as an FTS5 query against turn_fts and
// returns the top-K matches with snippet highlights. We sanitise the query
// (drop FTS5 operator chars) so freeform prompts don't error.
func queryHits(ctx context.Context, db *sql.DB, project, raw string, limit int) ([]queryHit, error) {
	q := sanitiseFTSQuery(raw)
	if q == "" {
		return nil, nil
	}
	args := []any{q}
	where := "turn_fts MATCH ?"
	if project != "" {
		where += " AND (sessions.project_dir = ? OR ? LIKE (sessions.project_dir || '/%'))"
		args = append(args, project, project)
	}
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT turns.role, snippet(turn_fts, 0, '<<', '>>', '...', 16)
		  FROM turn_fts
		  JOIN turns    ON turn_fts.rowid = turns.rowid
		  JOIN sessions ON turns.session_id = sessions.id
		 WHERE %s
		 ORDER BY rank, COALESCE(turns.ts, 0) DESC
		 LIMIT ?`, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queryHit
	for rows.Next() {
		var h queryHit
		if err := rows.Scan(&h.role, &h.excerpt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// sanitiseFTSQuery turns a freeform prompt into a permissive FTS5 query.
// Strips operator chars, drops very common stopwords + tokens < 3 chars,
// caps at 12 tokens, joins with OR so any token can match. (Default FTS5
// query syntax is AND across tokens, which is too strict for context-prime
// recall — we want recall, not precision.)
var ftsBadChars = regexp.MustCompile(`[^\p{L}\p{N}\s'-]`)

var ftsStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "you": true,
	"are": true, "can": true, "this": true, "that": true, "from": true,
	"how": true, "does": true, "did": true, "was": true, "but": true,
	"not": true, "have": true, "has": true, "what": true, "when": true,
	"why": true, "where": true, "who": true, "will": true, "would": true,
}

func sanitiseFTSQuery(s string) string {
	s = ftsBadChars.ReplaceAllString(s, " ")
	tokens := strings.Fields(strings.ToLower(s))

	out := make([]string, 0, len(tokens))
	seen := map[string]bool{}
	for _, t := range tokens {
		if len(t) < 3 || ftsStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		// Quote each token to avoid FTS5 special-character interpretation.
		out = append(out, `"`+t+`"`)
		if len(out) >= 12 {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " OR ")
}

func recentFiles(ctx context.Context, db *sql.DB, project string, cutoff int64, limit int) ([]fileRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT path, COUNT(*) AS n
		   FROM files
		  WHERE project_dir = ? AND COALESCE(ts, 0) >= ?
		  GROUP BY path
		  ORDER BY MAX(COALESCE(ts, 0)) DESC, n DESC
		  LIMIT ?`,
		project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.path, &f.count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
