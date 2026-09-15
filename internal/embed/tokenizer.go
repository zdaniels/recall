package embed

import (
	"encoding/json"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Tokenizer is a minimal BERT WordPiece tokenizer driven by a HuggingFace
// `tokenizer.json` file. We implement only what all-MiniLM-L6-v2 needs:
// BertNormalizer + BertPreTokenizer + WordPiece + simple template
// post-processing ([CLS] ... [SEP]). Anything fancier and we'd reach for
// `github.com/sugarme/tokenizer`; for one model it's cheaper to keep a
// focused 200-line implementation than the dep tree.
//
// Output is suitable for direct feed into BERT-style ONNX models:
// (input_ids, attention_mask, token_type_ids), all int64, padded to a
// fixed length per Encode call.
type Tokenizer struct {
	vocab        map[string]int
	unkID        int64
	clsID        int64
	sepID        int64
	padID        int64
	contPrefix   string
	maxLen       int
	lowercase    bool
	stripAccents bool
	cleanText    bool
}

func LoadTokenizer(path string, maxLen int) (*Tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Normalizer struct {
			Type         string `json:"type"`
			Lowercase    *bool  `json:"lowercase"`
			StripAccents *bool  `json:"strip_accents"`
			CleanText    *bool  `json:"clean_text"`
		} `json:"normalizer"`
		Model struct {
			Type              string         `json:"type"`
			Vocab             map[string]int `json:"vocab"`
			UnkToken          string         `json:"unk_token"`
			ContSubwordPrefix string         `json:"continuing_subword_prefix"`
		} `json:"model"`
		AddedTokens []struct {
			ID      int    `json:"id"`
			Content string `json:"content"`
		} `json:"added_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	t := &Tokenizer{
		vocab:      raw.Model.Vocab,
		contPrefix: raw.Model.ContSubwordPrefix,
		maxLen:     maxLen,
		// BertNormalizer defaults: lowercase=true, strip_accents=true (auto),
		// clean_text=true, handle_chinese_chars=true. We follow those.
		lowercase:    raw.Normalizer.Lowercase == nil || *raw.Normalizer.Lowercase,
		stripAccents: raw.Normalizer.StripAccents == nil || *raw.Normalizer.StripAccents,
		cleanText:    raw.Normalizer.CleanText == nil || *raw.Normalizer.CleanText,
	}
	if t.contPrefix == "" {
		t.contPrefix = "##"
	}

	// Resolve the special-token IDs from added_tokens (more reliable than
	// looking them up in the vocab map, since the vocab keys are byte-exact).
	for _, st := range raw.AddedTokens {
		switch st.Content {
		case "[CLS]":
			t.clsID = int64(st.ID)
		case "[SEP]":
			t.sepID = int64(st.ID)
		case "[PAD]":
			t.padID = int64(st.ID)
		case "[UNK]":
			t.unkID = int64(st.ID)
		}
	}
	return t, nil
}

// Encode returns (inputIDs, attentionMask, tokenTypeIDs) padded to len(inputIDs).
// Length is min(maxLen, actual). Caller can rely on len(inputIDs) ==
// len(attentionMask) == len(tokenTypeIDs).
func (t *Tokenizer) Encode(text string) (ids, mask, types []int64) {
	text = t.normalize(text)
	words := t.preTokenize(text)

	tokens := make([]int64, 0, len(words)+2)
	tokens = append(tokens, t.clsID)
	for _, w := range words {
		tokens = append(tokens, t.wordpiece(w)...)
	}
	tokens = append(tokens, t.sepID)

	// Truncate while preserving [SEP] at the end.
	if t.maxLen > 0 && len(tokens) > t.maxLen {
		tokens = tokens[:t.maxLen]
		tokens[len(tokens)-1] = t.sepID
	}

	ids = tokens
	mask = make([]int64, len(tokens))
	types = make([]int64, len(tokens))
	for i := range mask {
		mask[i] = 1
	}
	return
}

func (t *Tokenizer) normalize(s string) string {
	if t.cleanText {
		// Strip control chars (categories Cc, Cf), normalise whitespace.
		var b strings.Builder
		for _, r := range s {
			if r == 0 || r == 0xfffd {
				continue
			}
			if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
				if r == '\t' || r == '\n' || r == '\r' {
					b.WriteByte(' ')
				}
				continue
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	if t.stripAccents {
		// NFD decomposes pre-composed characters; combining marks (Mn) drop.
		s = norm.NFD.String(s)
		var b strings.Builder
		for _, r := range s {
			if !unicode.Is(unicode.Mn, r) {
				b.WriteRune(r)
			}
		}
		s = b.String()
	}
	if t.lowercase {
		s = strings.ToLower(s)
	}
	return s
}

// preTokenize implements BertPreTokenizer: split on whitespace, then make
// each punctuation rune its own token, and add spaces around CJK chars
// (which BERT treats as standalone tokens).
func (t *Tokenizer) preTokenize(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r) || isCJK(r):
			flush()
			words = append(words, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// wordpiece does greedy longest-match against the vocab. Continuation
// pieces get the `##` prefix. Returns [unkID] for unknown words rather
// than splitting unrecognisable runes.
func (t *Tokenizer) wordpiece(word string) []int64 {
	// Whole-word fast path.
	if id, ok := t.vocab[word]; ok {
		return []int64{int64(id)}
	}
	runes := []rune(word)
	if len(runes) > 100 {
		return []int64{t.unkID}
	}
	var out []int64
	start := 0
	for start < len(runes) {
		end := len(runes)
		matched := false
		for end > start {
			sub := string(runes[start:end])
			if start > 0 {
				sub = t.contPrefix + sub
			}
			if id, ok := t.vocab[sub]; ok {
				out = append(out, int64(id))
				start = end
				matched = true
				break
			}
			end--
		}
		if !matched {
			return []int64{t.unkID}
		}
	}
	return out
}

// isPunct matches BERT's notion of punctuation: ASCII !-/, :-@, [-`, {-~,
// plus Unicode punctuation categories. Per the BERT source this is broader
// than `unicode.IsPunct` alone.
func isPunct(r rune) bool {
	if (r >= '!' && r <= '/') ||
		(r >= ':' && r <= '@') ||
		(r >= '[' && r <= '`') ||
		(r >= '{' && r <= '~') {
		return true
	}
	return unicode.Is(unicode.P, r)
}

// isCJK is the CJK union (CJK Unified Ideographs + Extensions A/B/C/D/E + radicals).
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF:
		return true
	case r >= 0x3400 && r <= 0x4DBF:
		return true
	case r >= 0x20000 && r <= 0x2A6DF:
		return true
	case r >= 0x2A700 && r <= 0x2B73F:
		return true
	case r >= 0x2B740 && r <= 0x2B81F:
		return true
	case r >= 0x2B820 && r <= 0x2CEAF:
		return true
	case r >= 0xF900 && r <= 0xFAFF:
		return true
	case r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}
