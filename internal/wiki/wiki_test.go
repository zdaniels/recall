package wiki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

// fakeAnthropic returns a server that always answers with the given summary
// text in the Anthropic Messages response shape.
func fakeAnthropic(t *testing.T, summary string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("missing x-api-key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"` + summary + `"}]}`))
	}))
}

func TestRunBuildsCard(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Seed a session + a turn mentioning @kong, and index the mention.
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		srcID, err := tx.UpsertSource("test", "/test")
		if err != nil {
			return err
		}
		if err := tx.UpsertSession(store.Session{ID: "s1", SourceID: srcID, ProjectDir: "/p"}); err != nil {
			return err
		}
		text := "@kong is our API gateway; config lives at infra/kong.yml and @bob owns it"
		if _, err := tx.InsertTurn(store.Turn{UUID: "t1", SessionID: "s1", Idx: 0, Role: "user", Ts: 1000, Text: text}); err != nil {
			return err
		}
		return entities.IndexInTx(tx, entities.KindTurn, "t1", text, 1000)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := fakeAnthropic(t, "Kong is the API gateway; config at infra/kong.yml; owned by Bob.")
	defer srv.Close()

	stats, err := Run(ctx, st, nil, Options{
		APIKey:      "test-key",
		Endpoint:    srv.URL,
		MinMentions: 1, // both @kong and @bob mentioned once
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Built != 2 {
		t.Fatalf("want 2 cards built (@kong, @bob), got %d (considered=%d skipped=%d errors=%d)",
			stats.Built, stats.Considered, stats.Skipped, stats.Errors)
	}

	card, found, err := st.GetEntityCard(ctx, "kong")
	if err != nil || !found {
		t.Fatalf("kong card missing: found=%v err=%v", found, err)
	}
	if card.Display != "kong" || card.Summary == "" {
		t.Fatalf("unexpected card: %+v", card)
	}

	// Idempotency: re-running without new mentions should rebuild nothing
	// (entities.last_seen is not > entity_cards.refreshed_at).
	stats2, err := Run(ctx, st, nil, Options{APIKey: "test-key", Endpoint: srv.URL, MinMentions: 1})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if stats2.Built != 0 {
		t.Fatalf("re-run should build 0 (no new mentions), built %d", stats2.Built)
	}
}

func TestRunSkipsInsufficient(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "w2.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		srcID, _ := tx.UpsertSource("test", "/test")
		_ = tx.UpsertSession(store.Session{ID: "s1", SourceID: srcID})
		text := "ping @ghost"
		if _, err := tx.InsertTurn(store.Turn{UUID: "t1", SessionID: "s1", Idx: 0, Role: "user", Ts: 1, Text: text}); err != nil {
			return err
		}
		return entities.IndexInTx(tx, entities.KindTurn, "t1", text, 1)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := fakeAnthropic(t, "(insufficient)")
	defer srv.Close()

	stats, err := Run(ctx, st, nil, Options{APIKey: "k", Endpoint: srv.URL, MinMentions: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Built != 0 || stats.Skipped != 1 {
		t.Fatalf("want built=0 skipped=1, got built=%d skipped=%d", stats.Built, stats.Skipped)
	}
	if _, found, _ := st.GetEntityCard(ctx, "ghost"); found {
		t.Fatalf("insufficient summary should not have produced a card")
	}
}

func TestRunNoAPIKey(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "w3.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := Run(ctx, st, nil, Options{}); err == nil {
		t.Fatalf("expected error when ANTHROPIC_API_KEY is unset")
	}
}
