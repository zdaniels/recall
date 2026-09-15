package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rerank promotes the most-relevant candidate from a small post-fusion
// shortlist using a single Haiku call. The model sees the query plus
// numbered candidate excerpts and returns the index of the best match
// (or "0" when none clearly answers the query, in which case we leave
// the original order alone).
//
// Why post-fusion rather than pre-RRF: at this stage we have ≤ topK
// candidates that survived FTS5 + cosine + temporal + keyword fusion.
// They're all "plausibly relevant"; the model only needs to break the
// tie at the top of the list. One API call per query, ~1k input tokens
// + ≤ 5 output tokens — order $0.001 per question on Haiku 4.5.
//
// Falls back gracefully:
//   - no ANTHROPIC_API_KEY → return hits unchanged, no error
//   - HTTP / parsing failure → return hits unchanged with the underlying error
//   - model returns invalid index → return hits unchanged, no error
//
// Network: one POST to api.anthropic.com/v1/messages. Same destination
// as `recall consolidate` and the HyDE expansion.

const (
	rerankModel       = "claude-haiku-4-5-20251001"
	rerankEndpoint    = "https://api.anthropic.com/v1/messages"
	rerankAPIVersion  = "2023-06-01"
	rerankMaxTokens   = 16
	rerankExcerptSize = 700 // chars per candidate; trimmed at word boundary if possible
	rerankTimeout     = 12 * time.Second
)

const rerankSystemPrompt = `You re-rank retrieval candidates for a memory system. The user provides a query and a numbered list of candidate excerpts. Identify which one most directly answers the query.

Respond with EXACTLY this format and nothing else:

<pick>N</pick>

where N is the 1-indexed number of the best candidate, or 0 if none clearly answers or several are equally relevant. Do not include explanation, reasoning, or any text outside the tags.`

// RerankOptions configures one rerank pass. Embedder/etc. unused — the
// rerank only needs the textual hits and the query.
type RerankOptions struct {
	// TopK is the number of candidates considered. The model sees these
	// and may promote any one to position 1; the rest of the original
	// fused order is preserved below them. 5 is a good default; 10
	// roughly doubles cost without dramatically lifting recall.
	TopK int

	// Model overrides rerankModel. Empty = default Haiku.
	Model string

	// APIKey overrides ANTHROPIC_API_KEY env. Empty = read from env.
	APIKey string
}

// Rerank reorders the supplied hits in-place: the candidate the model
// picks (if any) gets pulled to index 0, everything else stays in
// original fused order. Returns the reordered slice (same underlying
// backing as the argument).
func Rerank(ctx context.Context, query string, hits []SearchHit, opts RerankOptions) ([]SearchHit, error) {
	if len(hits) == 0 || strings.TrimSpace(query) == "" {
		return hits, nil
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 5
	}
	if topK > len(hits) {
		topK = len(hits)
	}
	excerpts := make([]string, topK)
	for i, h := range hits[:topK] {
		excerpts[i] = hitText(h)
	}
	pick, err := PickBestExcerpt(ctx, query, excerpts, opts)
	if err != nil {
		return hits, err
	}
	if pick <= 0 || pick > topK || pick == 1 {
		return hits, nil
	}
	// Promote hits[pick-1] to position 0; preserve relative order of the rest.
	chosen := hits[pick-1]
	copy(hits[1:pick], hits[0:pick-1])
	hits[0] = chosen
	return hits, nil
}

// PickBestExcerpt is the low-level rerank: given a query and a list of
// candidate excerpts, ask Haiku which one most directly answers the
// query. Returns the 1-indexed pick (1..len(excerpts)) or 0 when no
// candidate clearly answers / the model declines to choose. Bench code
// uses this directly to avoid building SearchHit objects when all it
// needs is the chosen rank.
//
// Returns (0, nil) — *not* an error — when:
//   - excerpts is empty / query is empty
//   - ANTHROPIC_API_KEY is unset (and opts.APIKey unset)
//   - model returns "0" (no clear winner)
//
// Returns (0, err) on actual failures (HTTP error, malformed model
// output, network timeout). Callers can treat both as "leave the
// original ranking alone".
func PickBestExcerpt(ctx context.Context, query string, excerpts []string, opts RerankOptions) (int, error) {
	if len(excerpts) == 0 || strings.TrimSpace(query) == "" {
		return 0, nil
	}
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return 0, nil
	}
	model := opts.Model
	if model == "" {
		model = rerankModel
	}
	prompt := buildRerankPrompt(query, excerpts)
	return callHaikuRerank(ctx, apiKey, model, prompt)
}

func buildRerankPrompt(query string, excerpts []string) string {
	var b strings.Builder
	b.WriteString("Query:\n")
	b.WriteString(strings.TrimSpace(query))
	b.WriteString("\n\nCandidates:\n")
	for i, e := range excerpts {
		excerpt := strings.TrimSpace(e)
		if len(excerpt) > rerankExcerptSize {
			cut := rerankExcerptSize
			if sp := strings.LastIndexByte(excerpt[:cut], ' '); sp > rerankExcerptSize/2 {
				cut = sp
			}
			excerpt = excerpt[:cut] + "..."
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, excerpt)
	}
	return b.String()
}

func hitText(h SearchHit) string {
	if h.Excerpt != "" {
		return h.Excerpt
	}
	if h.Text != "" {
		return h.Text
	}
	return ""
}

func callHaikuRerank(ctx context.Context, apiKey, model, prompt string) (int, error) {
	body, _ := json.Marshal(haikuRequest{
		Model:     model,
		MaxTokens: rerankMaxTokens,
		System:    rerankSystemPrompt,
		Messages:  []haikuMessage{{Role: "user", Content: prompt}},
	})
	ctx, cancel := context.WithTimeout(ctx, rerankTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", rerankEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", rerankAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("anthropic %s: %s", resp.Status, string(respBody))
	}
	var parsed haikuResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, err
	}
	if parsed.Error != nil {
		return 0, fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type != "text" {
			continue
		}
		n, ok := parsePick(c.Text)
		if !ok {
			return 0, fmt.Errorf("rerank: no recognisable pick in model response %q", truncate(c.Text, 80))
		}
		return n, nil
	}
	return 0, fmt.Errorf("rerank: no text content in model response")
}

// pickRE matches the strict <pick>N</pick> form the system prompt
// requests. Tolerant of whitespace inside the tag.
var pickRE = regexp.MustCompile(`<pick>\s*(\d+)\s*</pick>`)

// parsePick extracts the model's choice from a response text. Tries the
// strict <pick>N</pick> form first; falls back to the LAST standalone
// integer in the text (preferred over first because models often
// preface answers with a recap like "Out of the 5 candidates, ..." —
// the trailing integer is the actual answer).
//
// Returns (n, true) on success; (0, false) when no integer is present
// at all.
func parsePick(text string) (int, bool) {
	if m := pickRE.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n, true
		}
	}
	// Fallback: scan for word-bounded integers, take the last one. This
	// handles "I'll go with 3" / "the answer is 7" / "Candidate 3 best matches".
	re := regexp.MustCompile(`\b(\d+)\b`)
	matches := re.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1][1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return 0, false
	}
	return n, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
