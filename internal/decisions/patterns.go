// Package decisions extracts candidate decisions from session turns via
// regex pattern matching. Patterns target high-precision phrasings only
// (explicit corrections, preferences, durable rules) to avoid noise — the
// goal is "miss rather than false-match." Output is stored at low salience
// (source='pattern') and the user or agent can promote.
package decisions

import (
	"regexp"
	"strings"

	"github.com/zdaniels/recall/internal/textutil"
)

// Candidate is one extracted decision.
type Candidate struct {
	Kind string // 'feedback' | 'preference' | 'fact'
	Text string
}

// Each entry is a regex with its kind. The captured group (1) is the rule.
// Patterns anchor on sentence boundaries (start of input or after .!?).
var patterns = []struct {
	re   *regexp.Regexp
	kind string
}{
	// "don't X" / "stop X"
	{regexp.MustCompile(`(?im)(?:^|[.!?]\s+)(?:no,?\s+)?(?:please\s+)?(?:stop|don'?t)\s+([a-z][^.!?\n]{4,200})`), "feedback"},
	// "let's go with X" / "let's use X"
	{regexp.MustCompile(`(?im)(?:^|[.!?]\s+)let'?s\s+(?:go\s+with|use)\s+([^.!?\n]{3,200})`), "preference"},
	// "from now on, X"
	{regexp.MustCompile(`(?im)(?:^|[.!?]\s+)from\s+now\s+on,?\s+([^.!?\n]{5,200})`), "feedback"},
	// "we decided X" / "we chose X"
	{regexp.MustCompile(`(?im)(?:^|[.!?]\s+)we(?:'ve)?\s+(?:decided|chose)\s+(?:to\s+)?([^.!?\n]{5,200})`), "fact"},
	// "remember (that) X" / "please remember X"
	{regexp.MustCompile(`(?im)(?:^|[.!?]\s+)(?:please\s+)?remember\s+(?:that\s+)?([^.!?\n]{5,200})`), "fact"},
}

const maxCandidatesPerTurn = 3

// Match returns up to maxCandidatesPerTurn candidates from text.
// Fenced + inline code blocks and <private>...</private> spans are stripped
// first so we never match patterns inside code or in user-marked private
// content.
func Match(text string) []Candidate {
	text = textutil.StripPrivate(text)
	text = stripCode(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	seen := map[string]bool{}
	var out []Candidate
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			cand := normaliseCandidate(m[1])
			if len(cand) < 5 {
				continue
			}
			key := strings.ToLower(cand)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Candidate{Kind: p.kind, Text: cand})
			if len(out) >= maxCandidatesPerTurn {
				return out
			}
		}
	}
	return out
}

func normaliseCandidate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".,;:!?")
	// collapse internal whitespace
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return s
}

var (
	fencedCode = regexp.MustCompile("(?s)```[^\n]*\n.*?```")
	inlineCode = regexp.MustCompile("`[^`\n]+`")
)

func stripCode(s string) string {
	s = fencedCode.ReplaceAllString(s, " ")
	s = inlineCode.ReplaceAllString(s, " ")
	return s
}
