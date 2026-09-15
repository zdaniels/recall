//go:build !darwin

package embed

import "fmt"

const appleAvailable = false

func newApple() (Embedder, error) {
	return nil, fmt.Errorf("apple embedder is darwin-only; use --provider ollama")
}
