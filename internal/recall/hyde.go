// Package recall implements retrieval helpers that sit above the raw
// embedder + FTS layers: Reciprocal Rank Fusion (rrf.go) for hybrid
// search, and HyDE (this file) for query expansion before embedding.
package recall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// HyDE = "Hypothetical Document Embeddings". Instead of embedding the
// raw query and comparing against memory embeddings, we ask Haiku to
// write a 1-sentence hypothetical answer first, and embed THAT. The
// hypothetical maps closer to the answer-space (declarative phrasing
// like "the binary is at /usr/local/bin/X") than the question-space
// ("where is the binary?"), so cosine similarity against stored
// declarative memories ranks more accurately.
//
// Network: one call to api.anthropic.com per uncached query. Same
// destination as `recall consolidate`. Uses ANTHROPIC_API_KEY env.
//
// Caching: in-process LRU keyed on SHA256 of the query. 1-hour TTL.
// Bounded at 256 entries (oldest evicted when full). A repeated query
// inside a session pays the API cost once.
//
// Graceful degradation: if no API key, network error, or empty
// response, returns the original query so the caller can still embed
// directly. HyDE is always-better-or-same versus baseline.

const (
	hydeModel        = "claude-haiku-4-5-20251001"
	hydeEndpoint     = "https://api.anthropic.com/v1/messages"
	hydeAPIVersion   = "2023-06-01"
	hydeMaxTokens    = 100
	hydeMinQueryChrs = 8
	hydeCacheSize    = 256
	hydeCacheTTL     = time.Hour

	hydeSystemPrompt = `You are helping retrieve memories from a developer's local memory system. Given a search query, write a single short sentence (under 25 words) that — if it existed in the developer's notes — would be a perfect match for the query. Output ONLY that sentence, nothing else. No quotes, no preambles, no explanations. Use plain declarative phrasing like the memory itself would.`
)

type hydeEntry struct {
	answer  string
	expires time.Time
	added   time.Time
}

type hydeCacheStore struct {
	mu      sync.Mutex
	entries map[string]hydeEntry
}

var globalCache = &hydeCacheStore{entries: make(map[string]hydeEntry)}

// Expand returns a hypothetical-answer rewrite of the query when
// Anthropic is configured; otherwise returns the original query.
// Always non-empty; safe to feed directly into an embedder.
func Expand(ctx context.Context, query string) string {
	q := strings.TrimSpace(query)
	if q == "" {
		return q
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return q
	}
	// Short queries are usually keywords (e.g. "auth"); HyDE doesn't help.
	if len(q) < hydeMinQueryChrs {
		return q
	}

	key := hashQuery(q)
	if hit, ok := globalCache.get(key); ok {
		return hit
	}

	ans, err := callHaikuHyDE(ctx, apiKey, q)
	if err != nil || ans == "" {
		return q
	}
	globalCache.set(key, ans)
	return ans
}

func hashQuery(q string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(q), " "))))
	return hex.EncodeToString(h[:16])
}

func (c *hydeCacheStore) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expires) {
		delete(c.entries, key)
		return "", false
	}
	return entry.answer, true
}

func (c *hydeCacheStore) set(key, answer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= hydeCacheSize {
		c.evictOldestLocked()
	}
	now := time.Now()
	c.entries[key] = hydeEntry{
		answer:  answer,
		expires: now.Add(hydeCacheTTL),
		added:   now,
	}
}

// evictOldestLocked drops the least-recently-added entry. Caller holds c.mu.
func (c *hydeCacheStore) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range c.entries {
		if first || e.added.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.added
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// CacheStats is exposed for `recall doctor` and tests.
type CacheStats struct {
	Size     int
	Capacity int
}

func HyDECacheStats() CacheStats {
	globalCache.mu.Lock()
	defer globalCache.mu.Unlock()
	return CacheStats{Size: len(globalCache.entries), Capacity: hydeCacheSize}
}

type haikuRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []haikuMessage `json:"messages"`
}

type haikuMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

func callHaikuHyDE(ctx context.Context, apiKey, query string) (string, error) {
	body, _ := json.Marshal(haikuRequest{
		Model:     hydeModel,
		MaxTokens: hydeMaxTokens,
		System:    hydeSystemPrompt,
		Messages:  []haikuMessage{{Role: "user", Content: query}},
	})
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", hydeEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", hydeAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic %s", resp.Status)
	}
	var parsed haikuResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type == "text" {
			return strings.TrimSpace(c.Text), nil
		}
	}
	return "", fmt.Errorf("no text in response")
}
