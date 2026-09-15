package embed

import (
	"testing"
)

// TestTokenizer_KnownPhrases verifies our tokenizer matches HuggingFace's
// reference output for a few hand-checked cases. The expected ids come from
// running `transformers.AutoTokenizer.from_pretrained("sentence-transformers/all-MiniLM-L6-v2")`
// on each phrase.
func TestTokenizer_KnownPhrases(t *testing.T) {
	tk, err := LoadTokenizer(TokenizerPath(), 128)
	if err != nil {
		t.Skipf("model not downloaded: %v", err)
	}

	cases := []struct {
		text   string
		want   []int64
		minLen int // some tokenisations are sensitive to whitespace; we sanity-check length
	}{
		// Reference: HF tokenizer for "hello" → [101, 7592, 102]
		{"hello", []int64{101, 7592, 102}, 3},
		// Multi-word
		{"hello world", []int64{101, 7592, 2088, 102}, 4},
		// Lowercase + accent strip — "Café" → "cafe" (vocab[cafe] = 7668)
		{"Café", []int64{101, 7668, 102}, 3},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			ids, mask, types := tk.Encode(c.text)
			if len(ids) < c.minLen {
				t.Fatalf("ids=%v (len %d), want at least %d", ids, len(ids), c.minLen)
			}
			if len(ids) != len(mask) || len(ids) != len(types) {
				t.Errorf("length mismatch: ids=%d mask=%d types=%d", len(ids), len(mask), len(types))
			}
			if c.want != nil {
				if len(ids) != len(c.want) {
					t.Errorf("ids=%v want %v", ids, c.want)
					return
				}
				for i := range ids {
					if ids[i] != c.want[i] {
						t.Errorf("ids[%d]=%d want %d", i, ids[i], c.want[i])
					}
				}
			}
			// First and last must always be CLS=101 / SEP=102.
			if ids[0] != 101 {
				t.Errorf("first token = %d, want CLS=101", ids[0])
			}
			if ids[len(ids)-1] != 102 {
				t.Errorf("last token = %d, want SEP=102", ids[len(ids)-1])
			}
		})
	}
}

func TestTokenizer_Truncation(t *testing.T) {
	tk, err := LoadTokenizer(TokenizerPath(), 8)
	if err != nil {
		t.Skipf("model not downloaded: %v", err)
	}
	long := "this is a long sentence with more than eight tokens for sure okay"
	ids, _, _ := tk.Encode(long)
	if len(ids) != 8 {
		t.Errorf("len=%d want 8", len(ids))
	}
	if ids[0] != 101 || ids[len(ids)-1] != 102 {
		t.Errorf("special tokens not preserved on truncation: ids=%v", ids)
	}
}
