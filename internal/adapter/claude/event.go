// Package claude parses Claude Code session JSONL files into ingest-ready events.
//
// The on-disk schema is unstable across Claude Code releases, so the parser is
// tolerant: unknown top-level event types and fields are ignored, and parse
// errors on a single line are logged and skipped rather than aborting the run.
package claude

import (
	"encoding/json"
	"time"
)

// Event is one decoded JSONL line. Fields beyond what we consume are ignored.
type Event struct {
	Type           string          `json:"type"`
	UUID           string          `json:"uuid"`
	SessionID      string          `json:"sessionId"`
	ParentUUID     string          `json:"parentUuid"`
	CWD            string          `json:"cwd"`
	GitBranch      string          `json:"gitBranch"`
	Version        string          `json:"version"`
	Entrypoint     string          `json:"entrypoint"`
	PromptID       string          `json:"promptId"`
	PermissionMode string          `json:"permissionMode"`
	Timestamp      time.Time       `json:"timestamp"`
	Message        json.RawMessage `json:"message"`
	AITitle        string          `json:"aiTitle"`
}

// Message is the inner shape of user/assistant events.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ContentBlock is one element of message.content[].
// Different content types populate different fields; check Type before using.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	IsError   bool            `json:"is_error"`    // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result (string or blocks)
}

// FileOp is a single file touch derived from a tool_use block.
type FileOp struct {
	Path string
	Op   string // read | edit | write
}
