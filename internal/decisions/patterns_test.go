package decisions

import (
	"strings"
	"testing"
)

func TestMatch_PositiveCases(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantKind string
		wantText string
	}{
		{"dont", "Don't mock the database in tests.", "feedback", "mock the database in tests"},
		{"please dont", "Please don't push to main without review.", "feedback", "push to main without review"},
		{"stop", "Stop using global state in handlers.", "feedback", "using global state in handlers"},
		{"lets go with", "Let's go with Postgres for the new service.", "preference", "Postgres for the new service"},
		{"lets use", "Let's use Postgres for the new service.", "preference", "Postgres for the new service"},
		{"from now on", "From now on, integration tests must hit real DBs.", "feedback", "integration tests must hit real DBs"},
		{"we decided", "We decided to ship the migration on Friday.", "fact", "ship the migration on Friday"},
		{"we chose", "We've chose to keep the old auth shim for now.", "fact", "keep the old auth shim for now"},
		{"remember that", "Remember that the token expires hourly.", "fact", "the token expires hourly"},
		{"please remember", "Please remember the migration is irreversible.", "fact", "the migration is irreversible"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Match(c.text)
			if len(got) != 1 {
				t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
			}
			if got[0].Kind != c.wantKind {
				t.Errorf("kind=%q want %q", got[0].Kind, c.wantKind)
			}
			if !strings.EqualFold(got[0].Text, c.wantText) {
				t.Errorf("text=%q want %q", got[0].Text, c.wantText)
			}
		})
	}
}

func TestMatch_NegativeCases(t *testing.T) {
	cases := []string{
		"Hi, can you help me with this?",
		"Just a normal question about the API.",
		"The function returns the result, not don't worry about caching.", // mid-sentence "don't"
		"What if we just rebuild?",                                        // not a directive
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if got := Match(c); len(got) != 0 {
				t.Errorf("expected no match, got %+v", got)
			}
		})
	}
}

func TestMatch_StripsCodeBlocks(t *testing.T) {
	text := "Here's the code:\n```go\n// don't break this\nfn main() {}\n```\nLet's go with Postgres."
	got := Match(text)
	if len(got) != 1 || got[0].Kind != "preference" {
		t.Fatalf("expected only the preference outside the code block, got %+v", got)
	}
}

func TestMatch_DedupesIdenticalCandidates(t *testing.T) {
	text := "Don't mock the database. Don't mock the database in tests please."
	got := Match(text)
	// Both match but second is a different string ("...in tests please") so we expect both
	// (or just one if the second wraps differently). Verify at least one and no duplicates.
	if len(got) == 0 {
		t.Fatal("expected matches")
	}
	seen := map[string]bool{}
	for _, c := range got {
		key := strings.ToLower(c.Text)
		if seen[key] {
			t.Errorf("duplicate candidate: %q", c.Text)
		}
		seen[key] = true
	}
}

func TestMatch_TurnLimitsAt3(t *testing.T) {
	text := "Don't A. Don't B. Don't C. Don't D. Don't E."
	got := Match(text)
	if len(got) > 3 {
		t.Errorf("expected ≤3 candidates, got %d", len(got))
	}
}
