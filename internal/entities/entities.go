// Package entities extracts and indexes @-mentioned entities from turn
// and decision text. Mentions are stored alongside the source row so
// retrieval can be scoped to "show me only items mentioning @bob" or
// "@xcs-web-app" without polluting the FTS index.
//
// Extraction is regex-only and intentionally lightweight (~µs per call):
// no tokenization, no LLM, no embedding. We match `@<name>` where <name>
// starts with a letter and may contain alnum, dot, underscore, hyphen up
// to 50 chars. We require the `@` to follow whitespace, line start, or
// punctuation that isn't part of an email/URL — so `foo@bar.com` does not
// produce an `@bar.com` entity.
//
// Names are normalized to lowercase for the canonical key; the original
// case is preserved as `display`. Trailing punctuation is stripped so
// "@bob," becomes "bob".
package entities

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/store"
)

// SourceKind tags whether a mention is on a turn or a decision row.
type SourceKind string

const (
	KindTurn     SourceKind = "turn"
	KindDecision SourceKind = "decision"
)

// mentionRE matches an @-prefixed entity name where the @ is not part
// of an email/URL. The leading group is whitespace, line start, or one
// of a small set of safe punctuation marks; the captured name starts
// with a letter and may contain alnum / `.` / `_` / `-`.
var mentionRE = regexp.MustCompile(`(?:^|[\s\(\[\{<,;:])@([A-Za-z][A-Za-z0-9._-]{0,49})`)

// Extract returns the de-duplicated, normalized entity names mentioned
// in `text`. The display map keeps the first-seen casing so callers can
// preserve user intent when displaying.
func Extract(text string) []Mention {
	if text == "" {
		return nil
	}
	matches := mentionRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]Mention{}
	out := make([]Mention, 0, len(matches))
	for _, m := range matches {
		raw := strings.TrimRight(m[1], ".,;:!?-_")
		if raw == "" {
			continue
		}
		name := strings.ToLower(raw)
		if _, ok := seen[name]; ok {
			continue
		}
		mention := Mention{Name: name, Display: raw}
		seen[name] = mention
		out = append(out, mention)
	}
	return out
}

// Mention is one entity reference recovered from text.
type Mention struct {
	Name    string // normalized lowercase
	Display string // original casing
}

// IndexMentions writes mentions in a fresh transaction. Use IndexInTx
// when you already hold a transaction (the common case during ingest —
// our SQLite is single-connection so nesting Tx calls deadlocks).
func IndexMentions(ctx context.Context, st *store.Store, kind SourceKind, sourceID string, text string, ts int64) error {
	if Extract(text) == nil {
		return nil
	}
	return st.Tx(ctx, func(tx *store.Tx) error {
		return IndexInTx(tx, kind, sourceID, text, ts)
	})
}

// IndexInTx writes mentions for `text` into the entity tables using the
// supplied transaction. Idempotent: PRIMARY KEY (entity_id, source_kind,
// source_id) means re-runs over the same source row are no-ops. Bumps
// mention_count + last_seen on the entity row so most-active entities
// are easy to surface.
func IndexInTx(tx *store.Tx, kind SourceKind, sourceID string, text string, ts int64) error {
	mentions := Extract(text)
	if len(mentions) == 0 {
		return nil
	}
	if ts == 0 {
		ts = time.Now().Unix()
	}
	for _, m := range mentions {
		var entityID int64
		err := tx.QueryRow(`
			INSERT INTO entities(name, display, first_seen, last_seen, mention_count)
			VALUES(?, ?, ?, ?, 1)
			ON CONFLICT(name) DO UPDATE SET
			  last_seen     = MAX(last_seen, excluded.last_seen),
			  mention_count = mention_count + 1
			RETURNING id
		`, m.Name, m.Display, ts, ts).Scan(&entityID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO entity_mentions(entity_id, source_kind, source_id, ts)
			VALUES(?, ?, ?, ?)
		`, entityID, string(kind), sourceID, ts); err != nil {
			return err
		}
	}
	return nil
}

// LookupID resolves an entity name (case-insensitive) to its row id.
// Returns 0 (not an error) if the name has never been mentioned.
func LookupID(ctx context.Context, st *store.Store, name string) (int64, error) {
	var id int64
	err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM entities WHERE name = ?`,
		strings.ToLower(strings.TrimSpace(name))).Scan(&id)
	if err != nil {
		// sql.ErrNoRows is the common case — caller treats as "no matches".
		if strings.Contains(err.Error(), "no rows") {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}
