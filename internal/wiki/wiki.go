// Package wiki builds the auto-curated entity wiki: one rolled-up, Haiku-
// distilled "card" per @-entity (a person, service, repo, or concept),
// refreshed as new mentions accumulate. It turns the raw entity_mentions
// index (schema v6) into a readable reference the agent can pull on demand
// via the recall_wiki MCP tool or `recall wiki --show`.
//
// Like consolidate / distill it calls Anthropic's Messages API directly and
// is a no-op (returns an error) when ANTHROPIC_API_KEY is unset. Idempotent:
// a card is only (re)built when the entity has new mentions since the card
// was last refreshed (entities.last_seen > entity_cards.refreshed_at).
//
// Cost: ~1 Haiku call per stale entity (~1k in + ~150 out tokens). Cheap;
// scales with the number of entities that gained mentions since last run.
package wiki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/embed"
	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

const (
	defaultModel    = "claude-haiku-4-5-20251001"
	defaultEndpoint = "https://api.anthropic.com/v1/messages"
	apiVersion      = "2023-06-01"
	maxTokens       = 300
	httpTimeout     = 30 * time.Second
	systemPrompt    = `You are writing a concise reference card about an entity (a person, service, repo, tool, or concept) for an engineer's long-term memory. You are given excerpts that mention it. Summarise what is DURABLY true about the entity in 2-4 sentences (<= 400 chars total): what it is, its role, key facts/paths/config/owners, and how it relates to other things. Ignore speculation, one-off chatter, and the question-asking around it. Write plain prose — no markdown, no bullets, no preamble like "This entity is". If the excerpts don't say anything durable, output exactly: (insufficient)`
)

// Options configures a wiki build.
type Options struct {
	APIKey            string
	Endpoint          string // override for tests / proxies
	Model             string
	MinMentions       int    // only build cards for entities with >= this many mentions
	Max               int    // cap entities processed per run
	MaxMentionsPerEnt int    // excerpts fed to the model per entity
	Project           string // unused for now; reserved for project-scoped wikis
	DryRun            bool
}

func (o *Options) defaults() {
	if o.Endpoint == "" {
		o.Endpoint = defaultEndpoint
	}
	if o.Model == "" {
		o.Model = defaultModel
	}
	if o.MinMentions == 0 {
		o.MinMentions = 3
	}
	if o.Max == 0 {
		o.Max = 50
	}
	if o.MaxMentionsPerEnt == 0 {
		o.MaxMentionsPerEnt = 30
	}
}

// Stats is the outcome of a wiki build.
type Stats struct {
	Considered int
	Built      int
	Skipped    int
	Errors     int
	Duration   time.Duration
}

type candidate struct {
	id      int64
	display string
}

// Run (re)builds cards for entities that have accumulated new mentions. emb
// may be nil — cards are still stored, just without an embedding.
func Run(ctx context.Context, st *store.Store, emb embed.Embedder, opts Options) (Stats, error) {
	opts.defaults()
	var stats Stats
	t0 := time.Now()
	if opts.APIKey == "" {
		return stats, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	cands, err := candidates(ctx, st, opts)
	if err != nil {
		return stats, err
	}
	stats.Considered = len(cands)

	for _, c := range cands {
		excerpts, err := mentions(ctx, st, c.id, opts.MaxMentionsPerEnt)
		if err != nil {
			stats.Errors++
			continue
		}
		if len(excerpts) == 0 {
			stats.Skipped++
			continue
		}
		summary, err := callHaiku(ctx, opts, c.display, excerpts)
		if err != nil {
			stats.Errors++
			continue
		}
		summary = strings.TrimSpace(summary)
		if summary == "" || summary == "(insufficient)" {
			stats.Skipped++
			continue
		}
		if opts.DryRun {
			stats.Built++
			continue
		}
		var enc []byte
		if emb != nil {
			if v, eerr := emb.Embed(ctx, summary); eerr == nil && len(v) > 0 {
				enc = vec.Encode(v)
			}
		}
		if err := st.UpsertEntityCard(ctx, c.id, summary, enc, len(excerpts)); err != nil {
			stats.Errors++
			continue
		}
		stats.Built++
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

// candidates returns entities worth a (re)build: enough mentions, and either
// no card yet or new mentions since the card was last refreshed.
func candidates(ctx context.Context, st *store.Store, opts Options) ([]candidate, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT e.id, e.display
		  FROM entities e
		  LEFT JOIN entity_cards c ON c.entity_id = e.id
		 WHERE e.mention_count >= ?
		   AND (c.entity_id IS NULL OR e.last_seen > c.refreshed_at)
		 ORDER BY e.mention_count DESC
		 LIMIT ?`, opts.MinMentions, opts.Max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.display); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// mentions gathers the most recent excerpt texts that mention an entity,
// pulling from both turns and decisions.
func mentions(ctx context.Context, st *store.Store, entityID int64, limit int) ([]string, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT COALESCE(t.text, d.text, '')
		  FROM entity_mentions m
		  LEFT JOIN turns t     ON m.source_kind = 'turn'     AND t.uuid = m.source_id
		  LEFT JOIN decisions d ON m.source_kind = 'decision' AND d.id = CAST(m.source_id AS INTEGER)
		 WHERE m.entity_id = ?
		 ORDER BY m.ts DESC
		 LIMIT ?`, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var txt string
		if err := rows.Scan(&txt); err != nil {
			return nil, err
		}
		txt = strings.TrimSpace(txt)
		if txt != "" {
			out = append(out, txt)
		}
	}
	return out, rows.Err()
}

// --- Anthropic Messages API (minimal, stdlib only) ---

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

func callHaiku(ctx context.Context, opts Options, display string, excerpts []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Entity: @%s\n\nExcerpts mentioning it:\n", display)
	for _, e := range excerpts {
		if len(e) > 500 {
			e = e[:500] + "…"
		}
		fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(e, "\n", " "))
	}
	body, _ := json.Marshal(messagesRequest{
		Model:     opts.Model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  []messageBlock{{Role: "user", Content: b.String()}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", opts.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", opts.APIKey)
	req.Header.Set("anthropic-version", apiVersion)

	client := &http.Client{Timeout: httpTimeout}
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
