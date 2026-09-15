package recall

import "testing"

func TestParsePick(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		// strict tag form (preferred, matches the system prompt instruction)
		{"<pick>3</pick>", 3, true},
		{"<pick> 7 </pick>", 7, true},
		{"   <pick>0</pick>\n", 0, true},

		// bare integer — tolerated as the previous parser did
		{"3", 3, true},
		{"3\n", 3, true},

		// explanatory prose with the answer at the end (last-int fallback)
		{"Looking at the candidates for the query, candidate 3 best matches.", 3, true},
		{"Out of the 5 candidates, I'll go with 2.", 2, true},
		{"The answer is 7.", 7, true},

		// mixed-with-tag — strict form wins regardless of where it appears
		{"Some prose before <pick>4</pick> and after.", 4, true},
		{"Reasoning... The pick is 5 but actually <pick>2</pick>.", 2, true},

		// no integer at all → not OK
		{"I cannot determine the answer from these candidates.", 0, false},
		{"", 0, false},

		// edge: zero is a valid pick (means "none clearly answers")
		{"<pick>0</pick>", 0, true},
		{"None of these answers the query, returning 0.", 0, true},
	}
	for _, c := range cases {
		got, ok := parsePick(c.in)
		if ok != c.wantOK {
			t.Errorf("parsePick(%q) ok=%v, want %v", c.in, ok, c.wantOK)
		}
		if ok && got != c.want {
			t.Errorf("parsePick(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
