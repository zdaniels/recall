package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zdaniels/recall/internal/textutil"
)

// ParseLine decodes one JSONL line. Returns nil for blank lines.
func ParseLine(line []byte) (*Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// DecodeMessage parses ev.Message into role + content blocks. Handles both the
// string-content shape (user turns sometimes use this) and the array-of-blocks
// shape (assistant turns and tool-bearing user turns).
func (ev *Event) DecodeMessage() (role string, blocks []ContentBlock, err error) {
	if len(ev.Message) == 0 {
		return "", nil, nil
	}
	var m Message
	if err := json.Unmarshal(ev.Message, &m); err != nil {
		return "", nil, fmt.Errorf("decode message: %w", err)
	}
	role = m.Role
	c := bytes.TrimSpace(m.Content)
	if len(c) == 0 {
		return role, nil, nil
	}
	switch c[0] {
	case '"':
		var s string
		if err := json.Unmarshal(c, &s); err != nil {
			return role, nil, err
		}
		return role, []ContentBlock{{Type: "text", Text: s}}, nil
	case '[':
		if err := json.Unmarshal(c, &blocks); err != nil {
			return role, nil, err
		}
		return role, blocks, nil
	}
	return role, nil, nil
}

// ExtractText concatenates the text-typed content blocks. Thinking blocks are
// skipped (verbose + private-feeling) and any <private>...</private> spans are
// removed before the result hits FTS or the decisions extractor.
func ExtractText(blocks []ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return textutil.StripPrivate(sb.String())
}

// ExtractToolUses returns just the tool_use blocks.
func ExtractToolUses(blocks []ContentBlock) []ContentBlock {
	var out []ContentBlock
	for _, b := range blocks {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

// FilesFromToolUse derives file ops from a tool_use block. Only the file-touching
// tools we care about are recognised; everything else returns nil.
func FilesFromToolUse(b ContentBlock) []FileOp {
	switch b.Name {
	case "Read":
		return readFilePath(b, "read")
	case "Edit", "NotebookEdit":
		return readFilePath(b, "edit")
	case "Write":
		return readFilePath(b, "write")
	}
	return nil
}

func readFilePath(b ContentBlock, op string) []FileOp {
	if len(b.Input) == 0 {
		return nil
	}
	var input struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(b.Input, &input); err != nil || input.FilePath == "" {
		return nil
	}
	return []FileOp{{Path: input.FilePath, Op: op}}
}

// LineReader walks lines from r starting at startOffset, calling fn for each
// non-blank line with the offset of the line and the trimmed bytes (no \n).
// Returns the final offset (== file length when fully consumed).
//
// Long lines (multi-MB) are handled — bufio.Reader.ReadBytes grows as needed.
func LineReader(r io.Reader, startOffset int64, fn func(offset int64, line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	offset := startOffset
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			lineLen := int64(len(line))
			trimmed := line
			if trimmed[len(trimmed)-1] == '\n' {
				trimmed = trimmed[:len(trimmed)-1]
			}
			if cbErr := fn(offset, trimmed); cbErr != nil {
				return offset, cbErr
			}
			offset += lineLen
		}
		if err == io.EOF {
			return offset, nil
		}
		if err != nil {
			return offset, err
		}
	}
}
