package recall

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
)

// GenerateParaphrases asks Haiku for N alternate phrasings of a memory.
// Used to broaden the embedding footprint of each decision so semantic
// search can find the same memory from very differently-worded queries
// ("our database choice" ↔ "we use Postgres" ↔ "what DB does this project run on").
//
// Returns the alternates only (not the original). Empty slice when
// ANTHROPIC_API_KEY is unset, the API call fails, or the response is
// unparseable — the caller should treat paraphrases as an *enrichment*,
// never a requirement.
func GenerateParaphrases(ctx context.Context, decisionText string, count int) ([]string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, nil
	}
	if count <= 0 {
		count = 4
	}
	text := strings.TrimSpace(decisionText)
	if len(text) < 8 {
		return nil, nil
	}

	system := fmt.Sprintf(
		`You are helping index memories for retrieval. Given a memory the developer has saved, generate %d alternate phrasings that someone might type as a search query when looking for this information later. Each phrasing should be a different angle: question form, keyword form, paraphrase, related-but-broader topic. Output ONLY a JSON array of strings, no other text. No preamble. No code fence. No commentary.`,
		count,
	)

	body, _ := json.Marshal(haikuRequest{
		Model:     hydeModel,
		MaxTokens: 220,
		System:    system,
		Messages:  []haikuMessage{{Role: "user", Content: text}},
	})

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", hydeEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", hydeAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("anthropic %s", resp.Status)
	}
	var parsed haikuResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type == "text" {
			return parseJSONStringArray(c.Text), nil
		}
	}
	return nil, nil
}

// parseJSONStringArray extracts a JSON array of strings from a model
// response, tolerating leading/trailing whitespace, surrounding code
// fences, and trailing prose. Returns empty slice on any failure.
func parseJSONStringArray(s string) []string {
	s = strings.TrimSpace(s)
	// Tolerate ```json … ``` fencing if the model slipped one in.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i > 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	// Find the first '[' and matching ']'.
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil
	}
	jsonPart := s[start : end+1]
	var arr []string
	if err := json.Unmarshal([]byte(jsonPart), &arr); err != nil {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, p := range arr {
		p = strings.TrimSpace(p)
		if p != "" && len(p) <= 300 {
			out = append(out, p)
		}
	}
	return out
}
