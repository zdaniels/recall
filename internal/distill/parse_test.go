package distill

import (
	"reflect"
	"testing"
)

func TestParseItems(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []extractItem
	}{
		{
			name: "bare json",
			in:   `{"items":[{"kind":"fact","text":"the dev cluster is dev-cluster in us-west-2"}]}`,
			want: []extractItem{{Kind: "fact", Text: "the dev cluster is dev-cluster in us-west-2"}},
		},
		{
			name: "code-fenced json",
			in:   "```json\n{\"items\":[{\"kind\":\"instruction\",\"text\":\"deploy by running make ship\"}]}\n```",
			want: []extractItem{{Kind: "instruction", Text: "deploy by running make ship"}},
		},
		{
			name: "leading prose then json",
			in:   "Here are the durable items I found:\n\n{\"items\":[{\"kind\":\"preference\",\"text\":\"macOS only\"}]}",
			want: []extractItem{{Kind: "preference", Text: "macOS only"}},
		},
		{
			name: "trailing prose after json",
			in:   `{"items":[{"kind":"fact","text":"x"}]}\n\nLet me know if you want more.`,
			want: []extractItem{{Kind: "fact", Text: "x"}},
		},
		{
			name: "empty items",
			in:   `{"items":[]}`,
			want: []extractItem{}, // unmarshalled empty array is non-nil
		},
		{
			name: "multiple items",
			in:   `{"items":[{"kind":"fact","text":"a"},{"kind":"instruction","text":"b"}]}`,
			want: []extractItem{
				{Kind: "fact", Text: "a"},
				{Kind: "instruction", Text: "b"},
			},
		},
		{
			name: "nested braces in string",
			in:   `{"items":[{"kind":"fact","text":"call gh api repos/o/r/contents/{path}"}]}`,
			want: []extractItem{{Kind: "fact", Text: "call gh api repos/o/r/contents/{path}"}},
		},
		{
			name: "escaped quotes",
			in:   `{"items":[{"kind":"fact","text":"set ENV=\"value\" before run"}]}`,
			want: []extractItem{{Kind: "fact", Text: `set ENV="value" before run`}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseItems(c.in)
			if err != nil {
				t.Fatalf("parseItems(%q) error: %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestParseItemsErrors(t *testing.T) {
	cases := []string{
		`not json at all`,
		`{"items":[{"kind":}]}`, // malformed
	}
	for _, c := range cases {
		_, err := parseItems(c)
		if err == nil {
			t.Errorf("parseItems(%q) should have errored", c)
		}
	}
}
