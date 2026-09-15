// Package consolidate compresses old episodic data into durable session
// summaries via the Anthropic Messages API (Claude Haiku 4.5 by default).
//
// The forgetting-curve sketch from DESIGN.md: episodic detail expires by
// default; salient stuff escalates. We approximate this for now by keeping
// raw turns intact (cheap to store) but rewriting `sessions.summary` for
// older sessions into a denser, more durable form than Claude Code's
// auto-generated `aiTitle`. Future work: optional pruning of raw turns
// once a summary exists; we explicitly do NOT do that here so users can
// always inspect the source.
//
// Calls Anthropic's API directly (no SDK) — POST /v1/messages with
// `x-api-key`. Network destination is the same one Claude Code already
// talks to, so security-controlled orgs that approve Claude Code should
// also approve this. API key read from ANTHROPIC_API_KEY env.
package consolidate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/store"
)

var osStderr = os.Stderr

const (
	defaultModel    = "claude-haiku-4-5-20251001"
	defaultEndpoint = "https://api.anthropic.com/v1/messages"
	apiVersion      = "2023-06-01"
	systemPrompt    = `You are summarising a coding session for long-term memory in 2–3 sentences (≤ 280 chars total). Focus on durable knowledge: what was decided, what was accomplished, the problem solved, and any preferences or constraints stated. Skip routine progress, polite chatter, and tool plumbing. Write in plain prose; do NOT use bullets or markdown. If the session is too thin to summarise meaningfully, output exactly "(no consolidation)".`
)

type Options struct {
	APIKey        string
	Endpoint      string // override for testing / proxies
	Model         string
	OlderThanDays int
	Max           int
	DBPath        string
}

type Stats struct {
	Considered int
	Updated    int
	Skipped    int
	Errors     int
	Duration   time.Duration
}

// Run pulls sessions older than OlderThanDays without a recent
// consolidation, asks Haiku for a tighter summary, and writes it back.
// Idempotent: only sessions where summary_consolidated_at < session updated_at
// are processed (so re-running picks up sessions with new turns).
func Run(ctx context.Context, st *store.Store, opts Options) (Stats, error) {
	t0 := time.Now()
	var stats Stats
	if opts.APIKey == "" {
		return stats, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	if opts.Model == "" {
		opts.Model = defaultModel
	}
	if opts.Endpoint == "" {
		opts.Endpoint = defaultEndpoint
	}
	if opts.OlderThanDays == 0 {
		opts.OlderThanDays = 7
	}
	if opts.Max == 0 {
		opts.Max = 25
	}
	cutoff := time.Now().AddDate(0, 0, -opts.OlderThanDays).Unix()

	rows, err := st.DB().QueryContext(ctx, `
		SELECT id, COALESCE(summary, ''), COALESCE(project_dir, ''), COALESCE(ended_at, started_at, 0)
		  FROM sessions
		 WHERE COALESCE(ended_at, started_at, 0) < ?
		   AND (summary_consolidated_at IS NULL
		        OR summary_consolidated_at < COALESCE(ended_at, started_at, 0))
		 ORDER BY ended_at ASC
		 LIMIT ?`, cutoff, opts.Max)
	if err != nil {
		return stats, err
	}
	type job struct {
		id      string
		summary string
		project string
		endedAt int64
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.summary, &j.project, &j.endedAt); err != nil {
			rows.Close()
			return stats, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	stats.Considered = len(jobs)

	for _, j := range jobs {
		digest, err := buildDigest(ctx, st, j.id, j.summary)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(stderr, "consolidate %s: digest: %v\n", j.id, err)
			continue
		}
		if digest == "" {
			stats.Skipped++
			continue
		}
		summary, err := callHaiku(ctx, opts, digest)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(stderr, "consolidate %s: haiku: %v\n", j.id, err)
			continue
		}
		summary = strings.TrimSpace(summary)
		if summary == "" || summary == "(no consolidation)" {
			// Mark consolidated_at anyway so we don't keep retrying
			_, _ = st.DB().ExecContext(ctx,
				`UPDATE sessions SET summary_consolidated_at = ? WHERE id = ?`,
				time.Now().Unix(), j.id)
			stats.Skipped++
			continue
		}
		if len(summary) > 500 {
			summary = summary[:500]
		}
		_, err = st.DB().ExecContext(ctx, `
			UPDATE sessions
			   SET summary = ?,
			       summary_source = 'auto-haiku',
			       summary_consolidated_at = ?
			 WHERE id = ?`,
			summary, time.Now().Unix(), j.id)
		if err != nil {
			stats.Errors++
			continue
		}
		stats.Updated++
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

// buildDigest assembles a compact representation of a session for the
// summariser: existing summary + first user prompt + last assistant
// response + most-touched files. Capped at ~6k chars so the API call
// stays cheap.
func buildDigest(ctx context.Context, st *store.Store, sessionID, existingSummary string) (string, error) {
	var sb strings.Builder
	if existingSummary != "" {
		fmt.Fprintf(&sb, "Existing title: %s\n\n", existingSummary)
	}

	// First user turn
	var firstUser string
	_ = st.DB().QueryRowContext(ctx, `
		SELECT text FROM turns
		 WHERE session_id = ? AND role = 'user' AND text != ''
		 ORDER BY idx LIMIT 1`, sessionID).Scan(&firstUser)
	if firstUser != "" {
		fmt.Fprintf(&sb, "First user prompt:\n%s\n\n", trim(firstUser, 1500))
	}

	// Last assistant turn
	var lastAsst string
	_ = st.DB().QueryRowContext(ctx, `
		SELECT text FROM turns
		 WHERE session_id = ? AND role = 'assistant' AND text != ''
		 ORDER BY idx DESC LIMIT 1`, sessionID).Scan(&lastAsst)
	if lastAsst != "" {
		fmt.Fprintf(&sb, "Last assistant turn:\n%s\n\n", trim(lastAsst, 2500))
	}

	// Most-edited files in this session
	rows, err := st.DB().QueryContext(ctx, `
		SELECT path, COUNT(*) FROM files
		 WHERE session_id = ? AND op IN ('edit', 'write')
		 GROUP BY path ORDER BY 2 DESC LIMIT 8`, sessionID)
	if err == nil {
		defer rows.Close()
		var fileLines []string
		for rows.Next() {
			var p string
			var n int
			if rows.Scan(&p, &n) == nil {
				fileLines = append(fileLines, fmt.Sprintf("- %s (×%d)", p, n))
			}
		}
		if len(fileLines) > 0 {
			fmt.Fprintf(&sb, "Files written/edited:\n%s\n", strings.Join(fileLines, "\n"))
		}
	}

	if sb.Len() < 80 {
		return "", nil // too thin to summarise
	}
	return sb.String(), nil
}

type messagesRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []messageBlock `json:"messages"`
}

type messageBlock struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func callHaiku(ctx context.Context, opts Options, digest string) (string, error) {
	body, _ := json.Marshal(messagesRequest{
		Model:     opts.Model,
		MaxTokens: 220,
		System:    systemPrompt,
		Messages: []messageBlock{
			{Role: "user", Content: digest},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", opts.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", opts.APIKey)
	req.Header.Set("anthropic-version", apiVersion)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var parsed messagesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("no text in response")
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stderr is a var (not os.Stderr direct) so tests could redirect.
var stderr io.Writer = stderrDefault{}

type stderrDefault struct{}

func (stderrDefault) Write(p []byte) (int, error) {
	return fmt.Fprint(osStderr, string(p))
}
