package entities

import (
	"reflect"
	"testing"
)

func TestExtract(t *testing.T) {
	cases := []struct {
		in   string
		want []string // normalized names in order
	}{
		{"", nil},
		{"hi @bob, please review @xcs-web-app", []string{"bob", "xcs-web-app"}},
		{"start of line @alice", []string{"alice"}},
		{"@Alice and @ALICE both", []string{"alice"}}, // dedupe by lowercase
		{"foo@bar.com is an email", nil},              // not preceded by safe context
		{"see (@charlie) for review", []string{"charlie"}},
		{"trailing punc: @dave, @eve.", []string{"dave", "eve"}},
		{"@a", []string{"a"}},
		{"@1starts-with-digit", nil}, // must start with a letter
		{"underscored @foo_bar names", []string{"foo_bar"}},
		{"dotted @file.go reference", []string{"file.go"}},
		{"twitter-style @user_name and @user-name", []string{"user_name", "user-name"}},
	}
	for _, c := range cases {
		got := Extract(c.in)
		var names []string
		for _, m := range got {
			names = append(names, m.Name)
		}
		if !reflect.DeepEqual(names, c.want) {
			t.Errorf("Extract(%q) = %v; want %v", c.in, names, c.want)
		}
	}
}

func TestExtract_PreservesDisplay(t *testing.T) {
	got := Extract("hi @AliceMcCool")
	if len(got) != 1 || got[0].Name != "alicemccool" || got[0].Display != "AliceMcCool" {
		t.Errorf("expected alicemccool/AliceMcCool, got %+v", got)
	}
}
