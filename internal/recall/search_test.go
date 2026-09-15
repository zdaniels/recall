package recall

import "testing"

func TestSanitizeFTS5(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"hello", "hello"},
		{"hello world", "hello world"},
		{"prefix*", "prefix*"},
		{"a_b_c", "a_b_c"},
		// the bug from Codex: hyphen in bareword was parsed as NOT
		{"cross-tool", `"cross-tool"`},
		{"cross-tool project architecture decisions Terraform memory tool",
			`"cross-tool" project architecture decisions Terraform memory tool`},
		// other meta chars that break MATCH
		{"foo:bar", `"foo:bar"`},
		{"foo(bar)", `"foo(bar)"`},
		{`he said "hi"`, `he said """hi"""`}, // internal quotes get doubled
		{"trailing-", `"trailing-"`},
		{"-leading", `"-leading"`},
	}
	for _, c := range cases {
		if got := sanitizeFTS5(c.in); got != c.want {
			t.Errorf("sanitizeFTS5(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestIsSimpleFTSToken(t *testing.T) {
	yes := []string{"hello", "Foo123", "snake_case", "prefix*", "a"}
	no := []string{"", "*", "cross-tool", "foo:bar", `he"x`, "()", "abc-"}
	for _, s := range yes {
		if !isSimpleFTSToken(s) {
			t.Errorf("isSimpleFTSToken(%q) = false; want true", s)
		}
	}
	for _, s := range no {
		if isSimpleFTSToken(s) {
			t.Errorf("isSimpleFTSToken(%q) = true; want false", s)
		}
	}
}
