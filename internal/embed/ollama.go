package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type ollamaEmbedder struct {
	baseURL string
	model   string
}

func newOllama(baseURL, model string) Embedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &ollamaEmbedder{baseURL: baseURL, model: model}
}

func (o *ollamaEmbedder) Name() string {
	return fmt.Sprintf("ollama:%s", o.model)
}

func (o *ollamaEmbedder) Dim() int { return 0 } // dynamic per-model

func (o *ollamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(buf)))
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	return out.Embedding, nil
}
