package textutil

import (
	"strings"
	"testing"
)

func TestStripPrivate(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello <private>secret</private> world", "hello   world"},
		{"<PRIVATE>case insensitive</PRIVATE>", " "},
		{"line1\n<private>\nmulti\nline\n</private>\nline2", "line1\n \nline2"},
		{"no tags here", "no tags here"},
		{"<private>only</private>", " "},
		{"a <private>x</private> b <private>y</private> c", "a   b   c"},
	}
	for _, c := range cases {
		got := StripPrivate(c.in)
		// normalize whitespace for comparison
		gotNorm := strings.Join(strings.Fields(got), " ")
		wantNorm := strings.Join(strings.Fields(c.want), " ")
		if gotNorm != wantNorm {
			t.Errorf("StripPrivate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
