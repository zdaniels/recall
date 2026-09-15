// Package embed provides a small provider abstraction over text-to-vector
// embedding backends. recall consumes this from `recall embed` and
// from the `recall_semantic_search` MCP tool.
//
// Providers shipped:
//
//   - apple   — Apple's NLEmbedding sentenceEmbeddingForLanguage (default
//     on darwin). On-device, no network, no asset download, no
//     third-party software. ~5ms per call on Apple Silicon.
//     512-dim vectors. Loses pure-Go cross-compile (CGO + ObjC).
//
//   - ollama  — local Ollama daemon. Cross-platform but adds an external
//     service dep. Useful as a fallback or for non-darwin users.
//
// Future providers (cloud APIs, ONNX in-process) plug in here.
package embed

import (
	"context"
	"errors"
	"fmt"
)

// Embedder turns text into a fixed-size float32 vector. Embedders must be
// safe for concurrent use across goroutines.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Name is used in logs + MCP tool responses so users know which
	// provider produced a vector.
	Name() string
	// Dim is the vector dimension. Returned vectors must always be this
	// length (callers can pre-allocate). 0 means unknown / dynamic.
	Dim() int
}

// Options is a superset of all provider configs. Each provider only reads
// the fields it cares about; unset fields fall back to documented defaults.
type Options struct {
	OllamaURL   string // default http://localhost:11434
	OllamaModel string // default nomic-embed-text
}

// New returns an Embedder for the named provider. Unknown providers and
// unsupported-on-this-OS combinations return an error rather than a stub
// — failing fast is better than silently producing zero vectors.
//
// Default selection: onnx if assets are present, else apple on darwin,
// else ollama. The onnx path gives noticeably better semantic quality
// for technical text; apple is the always-available fallback before the
// user runs `recall download-model`.
func New(provider string, opts Options) (Embedder, error) {
	switch provider {
	case "onnx":
		return newONNX()
	case "apple":
		return newApple()
	case "ollama":
		return newOllama(opts.OllamaURL, opts.OllamaModel), nil
	case "":
		if OnnxAssetsAvailable() {
			if e, err := newONNX(); err == nil {
				return e, nil
			}
			// fall through if init failed for any reason
		}
		if appleAvailable {
			return newApple()
		}
		return newOllama(opts.OllamaURL, opts.OllamaModel), nil
	}
	return nil, fmt.Errorf("unknown embedding provider %q (try onnx|apple|ollama)", provider)
}

// ErrNoEmbedder is returned by callers that didn't successfully initialise
// any provider.
var ErrNoEmbedder = errors.New("no embedding provider configured")
