// Package distill is the conversation-to-decision compactor: read a batch
// of recent turns, hand them to Haiku with a tight extraction prompt, and
// write back any durable infrastructure facts / preferences / runbook
// procedures it surfaces. Idempotent — same content turned into a
// decision twice deduplicates via store.Tx.InsertDecisionIfNew.
//
// The agent does some of this inline already (the rules tell it to call
// record_decision when the user says something durable), but in practice
// agents are conservative about distilling — they treat infra discovered
// in passing as task-specific. This pass is the asynchronous catch-up:
// scans the firehose of conversation turns and pulls out the few facts
// that should have been decisions.
//
// Cost: ~1k input + ~200 output tokens per ~20-turn batch on Haiku 4.5.
// 1000 turns ≈ 50 batches ≈ ~$0.30. Scales linearly. Falls back to a
// no-op when ANTHROPIC_API_KEY is unset (returns count=0, no error).
package distill

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/store"
)

const (
	model       = "claude-haiku-4-5-20251001"
	endpoint    = "https://api.anthropic.com/v1/messages"
	apiVersion  = "2023-06-01"
	maxTokens   = 800
	httpTimeout = 30 * time.Second

	defaultBatchSize     = 20  // turns per Haiku call
	defaultMaxTurns      = 500 // safety cap per `recall distill` invocation
	defaultMinTurnLength = 40  // skip turns shorter than this — usually "ok", "yes", filler
)

const systemPrompt = `You read a batch of conversation turns from an agentic coding session and extract durable facts the human is likely to ask about again. Return ONLY items the user benefits from remembering across sessions — infrastructure details, runbook procedures, hard preferences. Skip anything task-specific or one-off.

Categories to capture:
- INFRASTRUCTURE FACTS: cluster names, namespaces, manifest paths, AWS profile names, IAM ARN patterns, deploy URLs, database hosts, repo paths.
- RUNBOOK PROCEDURES: "to deploy X, run Y then Z" / "the recovery procedure is …" / "rotate by …".
- HARD PREFERENCES: "we always use Postgres", "macOS-only", "never push to main without review".

Categories to SKIP:
- Task chatter ("ok", "let me check", code review back-and-forth).
- One-off task instructions ("for this PR, also add the test", "in this script, change the limit").
- Things that look like preferences but are actually mid-task decisions ("let's use 16Gi for now" — that's a setting, not a preference).

Return STRICT JSON, no prose, no markdown fences:

{"items":[
  {"kind":"fact","text":"the dev EKS cluster is dev-cluster in us-west-2"},
  {"kind":"instruction","text":"to bump runner memory: edit the runners manifest at infra/github-runners/scaleset/autoscalingrunnerset.yaml; keep request==limit for Guaranteed QoS"},
  {"kind":"preference","text":"use git ls-remote --tags rather than the GitHub REST API for tag lookups in CI scripts (rate-limit avoidance)"}
]}

Empty result is fine: {"items":[]}.

Constraints:
- text MUST be self-contained: a future reader sees only that line, not the surrounding conversation.
- text MUST be <= 240 characters.
- kind is one of: fact, instruction, preference.
- Never include question-shaped text — extract the answer, not the question.
- Skip items that don't generalize beyond the immediate task.`

// Options configures one distill run.
type Options struct {
	Days      int    // lookback window. 0 = since last run.
	BatchSize int    // turns per Haiku call. 0 = defaultBatchSize.
	MaxTurns  int    // safety cap on turns to process. 0 = defaultMaxTurns.
	APIKey    string // override ANTHROPIC_API_KEY env. Empty = read from env.
	Model     string // override Haiku model id. Empty = default.
	DryRun    bool   // when true, log the would-be decisions but don't write.
	Verbose   io.Writer
}

// Result is the aggregate output of one distill run.
type Result struct {
	TurnsRead    int
	TurnsScanned int
	BatchesSent  int
	Items        int
	Inserted     int
	Errors       int
	Duration     time.Duration
}

// Run scans recent turns from `st` and writes the extracted decisions back.
func Run(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	t0 := time.Now()
	res := Result{}

	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		// No-op cleanly — caller can still report "0 items" and "skipped".
		res.Duration = time.Since(t0)
		return res, nil
	}
	mdl := opts.Model
	if mdl == "" {
		mdl = model
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	turns, err := loadCandidateTurns(ctx, st.DB(), opts.Days, maxTurns)
	if err != nil {
		return res, fmt.Errorf("load turns: %w", err)
	}
	res.TurnsRead = len(turns)

	for i := 0; i < len(turns); i += batchSize {
		end := i + batchSize
		if end > len(turns) {
			end = len(turns)
		}
		batch := turns[i:end]
		batchUUIDs := make([]string, 0, len(batch))
		var sb strings.Builder
		for j, t := range batch {
			if len(t.text) < defaultMinTurnLength {
				continue
			}
			res.TurnsScanned++
			fmt.Fprintf(&sb, "[%d] %s (%s, project=%s):\n%s\n\n",
				j+1, t.role, t.ts.UTC().Format("2006-01-02"),
				shortProject(t.projectDir),
				truncate(t.text, 1500))
			batchUUIDs = append(batchUUIDs, t.uuid)
		}
		if sb.Len() == 0 {
			markDistilled(ctx, st, batch)
			continue
		}

		items, err := callHaiku(ctx, apiKey, mdl, sb.String())
		res.BatchesSent++
		if err != nil {
			res.Errors++
			if opts.Verbose != nil {
				fmt.Fprintf(opts.Verbose, "batch %d-%d: %v\n", i, end, err)
			}
			continue
		}
		res.Items += len(items)

		// Pick a representative project_dir for the batch — the most-frequent
		// non-empty one. Cross-project batches are uncommon (turns from one
		// session generally share a project_dir) but if it happens we'd
		// rather scope global than guess wrong.
		project := mostFrequentProject(batch)

		for _, it := range items {
			if !validKind(it.Kind) || strings.TrimSpace(it.Text) == "" {
				continue
			}
			if opts.DryRun {
				if opts.Verbose != nil {
					fmt.Fprintf(opts.Verbose, "  [dry-run] %s: %s\n", it.Kind, it.Text)
				}
				continue
			}
			if err := st.Tx(ctx, func(tx *store.Tx) error {
				_, ins, err := tx.InsertDecisionIfNew(store.Decision{
					ProjectDir: project,
					Ts:         time.Now().Unix(),
					Kind:       it.Kind,
					Text:       it.Text,
					Source:     "distilled",
					Salience:   1.2, // between pattern (0.5) and tool/cli (1.5)
				})
				if err != nil {
					return err
				}
				if ins {
					res.Inserted++
				}
				return nil
			}); err != nil {
				res.Errors++
			}
		}

		markDistilled(ctx, st, batch)
	}

	res.Duration = time.Since(t0)
	return res, nil
}

type extractItem struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type haikuResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func callHaiku(ctx context.Context, apiKey, mdl, batch string) ([]extractItem, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      mdl,
		"max_tokens": maxTokens,
		"system":     systemPrompt,
		"messages":   []map[string]string{{"role": "user", "content": batch}},
	})
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", apiVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("anthropic %s: %s", resp.Status, truncate(string(respBody), 200))
	}
	var parsed haikuResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type != "text" {
			continue
		}
		return parseItems(c.Text)
	}
	return nil, nil
}

// parseItems is tolerant of code-fenced JSON, leading prose, trailing
// junk. Strips the most common Haiku-isms before unmarshalling.
func parseItems(s string) ([]extractItem, error) {
	s = strings.TrimSpace(s)
	// Drop ``` fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// If there's prose before the JSON object, jump to the first '{'.
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	// Trim trailing prose past the matching '}' (cheap depth scan).
	if end := matchObjectEnd(s); end > 0 && end < len(s) {
		s = s[:end]
	}
	var wrap struct {
		Items []extractItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(s), &wrap); err != nil {
		return nil, fmt.Errorf("parse extractor json: %w (raw=%q)", err, truncate(s, 200))
	}
	return wrap.Items, nil
}

func matchObjectEnd(s string) int {
	if len(s) == 0 || s[0] != '{' {
		return 0
	}
	depth := 0
	inStr := false
	esc := false
	for i, r := range s {
		if esc {
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		if r == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

func validKind(k string) bool {
	switch k {
	case "fact", "instruction", "preference":
		return true
	}
	return false
}

type candidateTurn struct {
	uuid       string
	role       string
	text       string
	ts         time.Time
	projectDir string
}

func loadCandidateTurns(ctx context.Context, db *sql.DB, days, maxTurns int) ([]candidateTurn, error) {
	args := []any{}
	where := "turns.distilled_at IS NULL AND turns.text IS NOT NULL AND length(turns.text) >= ?"
	args = append(args, defaultMinTurnLength)
	if days > 0 {
		where += " AND turns.ts >= ?"
		args = append(args, time.Now().Add(-time.Duration(days)*24*time.Hour).Unix())
	}
	args = append(args, maxTurns)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT turns.uuid, turns.role, turns.text, COALESCE(turns.ts, 0),
		       COALESCE(sessions.project_dir, '')
		  FROM turns
		  LEFT JOIN sessions ON sessions.id = turns.session_id
		 WHERE %s
		 ORDER BY turns.ts ASC
		 LIMIT ?
	`, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidateTurn
	for rows.Next() {
		var c candidateTurn
		var ts int64
		if err := rows.Scan(&c.uuid, &c.role, &c.text, &ts, &c.projectDir); err != nil {
			return nil, err
		}
		if ts > 0 {
			c.ts = time.Unix(ts, 0)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func markDistilled(ctx context.Context, st *store.Store, batch []candidateTurn) {
	if len(batch) == 0 {
		return
	}
	now := time.Now().Unix()
	_ = st.Tx(ctx, func(tx *store.Tx) error {
		uuids := make([]any, 0, len(batch)+1)
		uuids = append(uuids, now)
		placeholders := make([]string, 0, len(batch))
		for _, t := range batch {
			placeholders = append(placeholders, "?")
			uuids = append(uuids, t.uuid)
		}
		_, err := tx.Exec(
			fmt.Sprintf(`UPDATE turns SET distilled_at = ? WHERE uuid IN (%s)`,
				strings.Join(placeholders, ",")),
			uuids...,
		)
		return err
	})
}

func mostFrequentProject(batch []candidateTurn) string {
	counts := map[string]int{}
	for _, t := range batch {
		if t.projectDir == "" {
			continue
		}
		counts[t.projectDir]++
	}
	var top string
	var n int
	for p, c := range counts {
		if c > n {
			top = p
			n = c
		}
	}
	return top
}

func shortProject(p string) string {
	if p == "" {
		return "(global)"
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
