// Package textutil provides small text-cleaning helpers shared across adapters
// and pattern detection. Kept tiny on purpose — anything more substantial
// should live where it's used.
package textutil

import "regexp"

// StripPrivate removes <private>...</private> blocks (case-insensitive,
// multiline) so anything wrapped in them never lands in FTS, decisions, or
// inject output. Users mark sensitive content inline and trust the
// indexer to filter it before persistence.
func StripPrivate(s string) string {
	return privateRE.ReplaceAllString(s, " ")
}

var privateRE = regexp.MustCompile(`(?is)<private>.*?</private>`)
