package recall

import "testing"

func TestFuse_SingleChannelMatchesInputOrder(t *testing.T) {
	in := Channel{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	got := Fuse([]Channel{in}, DefaultK, 0)
	want := []string{"1", "2", "3"}
	for i, h := range got {
		if h.ID != want[i] {
			t.Errorf("got[%d]=%s want %s", i, h.ID, want[i])
		}
	}
}

func TestFuse_DocInBothChannelsBeatsDocInOnly(t *testing.T) {
	chA := Channel{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	chB := Channel{{ID: "4"}, {ID: "2"}, {ID: "5"}}
	got := Fuse([]Channel{chA, chB}, DefaultK, 0)
	if got[0].ID != "2" {
		t.Errorf("expected doc 2 ranked first, got %s (full: %+v)", got[0].ID, got)
	}
}

func TestFuse_LimitTruncates(t *testing.T) {
	chA := Channel{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}}
	got := Fuse([]Channel{chA}, DefaultK, 2)
	if len(got) != 2 {
		t.Errorf("len=%d want 2", len(got))
	}
}

func TestFuse_EmptyInput(t *testing.T) {
	if got := Fuse(nil, DefaultK, 0); got != nil && len(got) != 0 {
		t.Errorf("nil input returned %v", got)
	}
	if got := Fuse([]Channel{}, DefaultK, 0); got != nil && len(got) != 0 {
		t.Errorf("empty input returned %v", got)
	}
}

func TestFuse_StableTieBreak(t *testing.T) {
	chA := Channel{{ID: "1"}}
	chB := Channel{{ID: "2"}}
	got := Fuse([]Channel{chA, chB}, DefaultK, 0)
	if got[0].ID != "1" {
		t.Errorf("expected doc 1 (channel A first), got %s", got[0].ID)
	}
}

func TestFuse_HeterogeneousNamespaces(t *testing.T) {
	// Hybrid retrieval: turn channel emits "turn:UUID" IDs, decision
	// channel emits "dec:N" IDs. RRF treats them as opaque strings,
	// so they coexist and rank by RRF score.
	turns := Channel{{ID: "turn:abc"}, {ID: "turn:def"}}
	decs := Channel{{ID: "dec:10"}, {ID: "dec:11"}}
	got := Fuse([]Channel{turns, decs}, DefaultK, 0)
	if len(got) != 4 {
		t.Errorf("len=%d want 4", len(got))
	}
	// Channels metadata should record which channel found each hit.
	for _, h := range got {
		if len(h.Channels) != 1 {
			t.Errorf("hit %s should be in 1 channel, got %v", h.ID, h.Channels)
		}
	}
}
