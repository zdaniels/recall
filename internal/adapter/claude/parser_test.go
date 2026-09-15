package claude

import (
	"strings"
	"testing"
)

func TestParseLine_UserEventStringContent(t *testing.T) {
	line := []byte(`{"parentUuid":null,"type":"user","uuid":"u-1","sessionId":"s","timestamp":"2026-04-30T05:00:00Z","cwd":"/p","message":{"role":"user","content":"hello"}}`)
	ev, err := ParseLine(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != "user" || ev.UUID != "u-1" || ev.SessionID != "s" || ev.CWD != "/p" {
		t.Fatalf("missing fields: %+v", ev)
	}
	if ev.Timestamp.IsZero() {
		t.Fatal("timestamp not parsed")
	}
	role, blocks, err := ev.DecodeMessage()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if role != "user" {
		t.Fatalf("role=%q", role)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Fatalf("blocks=%+v", blocks)
	}
}

func TestParseLine_AssistantWithToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"u-2","sessionId":"s","cwd":"/p","message":{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"reading"},{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/p/README.md"}}]}}`)
	ev, err := ParseLine(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, blocks, err := ev.DecodeMessage()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := ExtractText(blocks); got != "reading" {
		t.Fatalf("text=%q (thinking should be skipped)", got)
	}
	tools := ExtractToolUses(blocks)
	if len(tools) != 1 || tools[0].Name != "Read" {
		t.Fatalf("tools=%+v", tools)
	}
	ops := FilesFromToolUse(tools[0])
	if len(ops) != 1 || ops[0].Path != "/p/README.md" || ops[0].Op != "read" {
		t.Fatalf("ops=%+v", ops)
	}
}

func TestParseLine_BlankAndWhitespace(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(""), []byte("   \t\n  ")} {
		ev, err := ParseLine(in)
		if err != nil || ev != nil {
			t.Fatalf("blank: ev=%v err=%v", ev, err)
		}
	}
}

func TestParseLine_Malformed(t *testing.T) {
	if _, err := ParseLine([]byte(`{not json`)); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestDecodeMessage_ArrayContentWithToolResult(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"u-3","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"ok"}]}}`)
	ev, _ := ParseLine(line)
	role, blocks, err := ev.DecodeMessage()
	if err != nil {
		t.Fatal(err)
	}
	if role != "user" {
		t.Fatalf("role=%q", role)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "tu-1" {
		t.Fatalf("blocks=%+v", blocks)
	}
}

func TestFilesFromToolUse_AllRecognisedOps(t *testing.T) {
	cases := []struct {
		name, op string
	}{
		{"Read", "read"},
		{"Edit", "edit"},
		{"Write", "write"},
		{"NotebookEdit", "edit"},
	}
	for _, c := range cases {
		b := ContentBlock{Type: "tool_use", Name: c.name, Input: []byte(`{"file_path":"/p/f"}`)}
		ops := FilesFromToolUse(b)
		if len(ops) != 1 || ops[0].Op != c.op {
			t.Fatalf("%s: ops=%+v", c.name, ops)
		}
	}
	if got := FilesFromToolUse(ContentBlock{Type: "tool_use", Name: "Bash", Input: []byte(`{"command":"ls"}`)}); got != nil {
		t.Fatalf("unrelated tool returned ops: %+v", got)
	}
	if got := FilesFromToolUse(ContentBlock{Type: "tool_use", Name: "Read", Input: []byte(`{}`)}); got != nil {
		t.Fatal("missing file_path should yield no ops")
	}
}

func TestLineReader_Offsets(t *testing.T) {
	in := "alpha\nbeta\n\ngamma\n"
	var got []string
	var offsets []int64
	final, err := LineReader(strings.NewReader(in), 0, func(off int64, line []byte) error {
		offsets = append(offsets, off)
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if final != int64(len(in)) {
		t.Fatalf("final=%d want %d", final, len(in))
	}
	want := []string{"alpha", "beta", "", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("lines=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: %q != %q", i, got[i], want[i])
		}
	}
	wantOff := []int64{0, 6, 11, 12}
	for i := range wantOff {
		if offsets[i] != wantOff[i] {
			t.Fatalf("offset %d: %d != %d", i, offsets[i], wantOff[i])
		}
	}
}
