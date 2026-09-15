// Package cline ingests Cline (VS Code extension by saoudrizwan/Anthropic)
// conversation history into recall.
//
// Cline stores every task under VS Code's globalStorage:
//
//	macOS:   ~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks/<taskId>/
//	Linux:   ~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/tasks/<taskId>/
//	Windows: %APPDATA%\Code\User\globalStorage\saoudrizwan.claude-dev\tasks\<taskId>\
//
// Each task directory contains:
//
//	api_conversation_history.json   ← Anthropic MessageParam[] — the wire we ingest
//	ui_messages.json                ← UI-level events (incl. tool calls/results)
//	task_metadata.json              ← title, model, totals
//	context_history.json
//
// taskId is `Date.now().toString()` — Unix ms — which gives us the
// session timestamp without parsing metadata.
//
// Mapping:
//
//	<taskId>                         → sessions.id (prefixed "cline:")
//	int(<taskId>) ms                  → sessions.started_at
//	api_conversation_history items   → turns (role + extracted text)
package cline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "cline"
const SessionPrefix = "cline:"

// DefaultRoot returns the platform's VS Code globalStorage tasks path
// for the Cline extension. Returns the macOS path on unknown platforms;
// callers should treat a "directory doesn't exist" result as "Cline
// isn't installed."
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "tasks")
	case "linux":
		// XDG_CONFIG_HOME if set, else ~/.config
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "tasks")
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "tasks")
	}
	return filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "tasks")
}

type IngestStats struct {
	RootExists  bool
	Tasks       int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

// Ingest scans every task directory under the Cline globalStorage and
// writes its conversation history into recall. Idempotent on re-run:
// per-task ingest_state tracks the last seen message index.
func Ingest(ctx context.Context, st *store.Store, root string) (IngestStats, error) {
	t0 := time.Now()
	var stats IngestStats

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		stats.Duration = time.Since(t0)
		return stats, nil
	}
	stats.RootExists = true

	var sourceID int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		id, err := tx.UpsertSource(Source, root)
		if err != nil {
			return err
		}
		sourceID = id
		return nil
	}); err != nil {
		return stats, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return stats, fmt.Errorf("read cline tasks dir: %w", err)
	}

	// Process newest first so partial ingests still get the freshest
	// material if interrupted. taskId is Unix ms; lexical sort works
	// because all values have the same digit width within any given
	// year. Reverse for newest-first.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		convPath := filepath.Join(root, taskID, "api_conversation_history.json")
		if _, err := os.Stat(convPath); err != nil {
			continue // task without conversation file; skip silently
		}
		stats.Tasks++

		if newS, newT, err := ingestTask(ctx, st, sourceID, taskID, convPath); err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: cline task %s: %v\n", taskID, err)
		} else {
			if newS {
				stats.NewSessions++
			}
			stats.NewTurns += newT
		}
	}

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkSourceIngested(sourceID)
	}); err != nil {
		return stats, err
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

// apiMsg matches the relevant subset of Anthropic.MessageParam written
// by Cline. content is unmarshaled into json.RawMessage so we can
// dispatch on string-vs-array shape ourselves.
type apiMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// uiMsg is the subset of Cline's ClineMessage we care about. ui_messages.json
// carries per-event Unix-ms timestamps and (when present) a back-reference
// to the corresponding api_conversation_history index — so we can stamp
// each api turn with its real wall-clock time instead of the task's
// start time. See apps/vscode/src/shared/ExtensionMessage.ts in the
// upstream repo for the full ClineMessage shape.
type uiMsg struct {
	Ts                       int64 `json:"ts"`
	ConversationHistoryIndex *int  `json:"conversationHistoryIndex,omitempty"`
}

// loadTurnTimestamps reads ui_messages.json next to api_conversation_history.json
// and builds a map from api-history index → wall-clock ts (Unix ms).
// Returns nil if the file doesn't exist — caller falls back to taskId
// as the timestamp.
func loadTurnTimestamps(taskDir string) map[int]int64 {
	uiPath := filepath.Join(taskDir, "ui_messages.json")
	raw, err := os.ReadFile(uiPath)
	if err != nil {
		return nil
	}
	var msgs []uiMsg
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil
	}
	out := make(map[int]int64, len(msgs))
	for _, m := range msgs {
		if m.ConversationHistoryIndex == nil || m.Ts == 0 {
			continue
		}
		// Multiple UI messages can reference the same api-history index
		// (e.g. when a streaming response goes through several partial
		// updates). Keep the EARLIEST ts so each turn reflects when its
		// content first appeared, not when the final partial settled.
		idx := *m.ConversationHistoryIndex
		if existing, ok := out[idx]; !ok || m.Ts < existing {
			out[idx] = m.Ts
		}
	}
	return out
}

func ingestTask(ctx context.Context, st *store.Store, sourceID int64, taskID, convPath string) (bool, int, error) {
	raw, err := os.ReadFile(convPath)
	if err != nil {
		return false, 0, err
	}
	var msgs []apiMsg
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return false, 0, fmt.Errorf("parse api_conversation_history: %w", err)
	}

	sessionID := SessionPrefix + taskID
	startedMs, _ := strconv.ParseInt(taskID, 10, 64) // Cline uses Date.now().toString()

	// Per-turn timestamps from the UI message log. Missing-file or
	// parse-error means we fall back to the task start time for every
	// turn (same behavior as before).
	taskDir := filepath.Dir(convPath)
	turnTs := loadTurnTimestamps(taskDir)
	// Track the latest turn timestamp we observe so we can update the
	// session's ended_at without re-reading the UI log later.
	endedMs := startedMs

	// Title: first user message, trimmed to one line.
	title := ""
	for _, m := range msgs {
		if strings.ToLower(m.Role) == "user" {
			text := extractText(m.Content)
			text = firstLine(text)
			if text != "" {
				title = truncate(text, 200)
				break
			}
		}
	}

	// Compute ended_at from the maximum per-turn ts we've found (or
	// fall back to startedMs if the UI log was missing).
	if turnTs != nil {
		for _, ts := range turnTs {
			if ts > endedMs {
				endedMs = ts
			}
		}
	}

	var newSession bool
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		prev, err := tx.GetIngestState(sourceID, "task:"+taskID)
		if err != nil {
			return err
		}
		if prev.LastOffset == 0 {
			newSession = true
		}
		return tx.UpsertSession(store.Session{
			ID:        sessionID,
			SourceID:  sourceID,
			StartedAt: startedMs,
			EndedAt:   endedMs,
			Summary:   title,
		})
	}); err != nil {
		return false, 0, err
	}

	// Find resumption point — ingest_state.last_offset = last seen
	// message index, so re-runs only walk new tail messages.
	var lastIdx int64
	_ = st.Tx(ctx, func(tx *store.Tx) error {
		prev, err := tx.GetIngestState(sourceID, "task:"+taskID)
		if err != nil {
			return err
		}
		lastIdx = prev.LastOffset
		return nil
	})

	newTurns := 0
	for i := int(lastIdx); i < len(msgs); i++ {
		m := msgs[i]
		text := extractText(m.Content)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		turnUUID := fmt.Sprintf("%s#%d", sessionID, i)
		// Prefer the per-turn ts from ui_messages.json; fall back to
		// the task's start time for messages that have no UI event.
		ts := startedMs
		if turnTs != nil {
			if v, ok := turnTs[i]; ok && v > 0 {
				ts = v
			}
		}
		ins, err := insertTurnAndIndex(ctx, st, store.Turn{
			UUID:      turnUUID,
			SessionID: sessionID,
			Idx:       i + 1,
			Role:      normalizeRole(m.Role),
			Ts:        ts,
			Text:      text,
		})
		if err != nil {
			return newSession, newTurns, err
		}
		if ins {
			newTurns++
		}
	}

	// Update high-water-mark
	if int64(len(msgs)) > lastIdx {
		_ = st.Tx(ctx, func(tx *store.Tx) error {
			return tx.SetIngestState(sourceID, "task:"+taskID, store.IngestState{
				LastOffset: int64(len(msgs)),
				LastLine:   len(msgs),
				LastUUID:   fmt.Sprintf("%s#%d", sessionID, len(msgs)-1),
			})
		})
	}
	return newSession, newTurns, nil
}

// extractText pulls human-readable text out of an Anthropic MessageParam's
// content field. content can be:
//   - a JSON string (the simple case)
//   - a JSON array of content blocks, each with a "type" discriminator —
//     "text" / "tool_use" / "tool_result" / "image" etc.
//
// We pull "text" blocks verbatim and summarize tool_use / tool_result so
// the conversation remains searchable without bloating the index with
// raw tool inputs/outputs.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String case
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Array of blocks
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text,omitempty"`
		Name    string          `json:"name,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, b.Text)
			}
		case "tool_use":
			if b.Name != "" {
				out = append(out, "[tool_use: "+b.Name+"]")
			}
		case "tool_result":
			// Tool results are nested content — best-effort extract.
			out = append(out, "[tool_result: "+truncate(string(b.Content), 120)+"]")
		case "image":
			out = append(out, "[image]")
		}
	}
	return strings.Join(out, "\n")
}

func insertTurnAndIndex(ctx context.Context, st *store.Store, t store.Turn) (bool, error) {
	var inserted bool
	err := st.Tx(ctx, func(tx *store.Tx) error {
		ins, err := tx.InsertTurn(t)
		if err != nil {
			return err
		}
		inserted = ins
		if ins {
			return entities.IndexInTx(tx, entities.KindTurn, t.UUID, t.Text, t.Ts)
		}
		return nil
	})
	return inserted, err
}

func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "user", "human":
		return "user"
	case "assistant", "model":
		return "assistant"
	case "system":
		return "system"
	default:
		return role
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
