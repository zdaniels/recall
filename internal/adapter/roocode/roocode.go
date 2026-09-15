// Package roocode ingests Roo Code (formerly Roo Cline) task history
// into recall.
//
// Roo Code is a Cline fork; the on-disk layout is identical except for
// the VS Code extension publisher slug — Roo's globalStorage lives at
//
//	macOS:   ~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks/<taskId>/
//	Linux:   ~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks/<taskId>/
//	Windows: %APPDATA%\Code\User\globalStorage\rooveterinaryinc.roo-cline\tasks\<taskId>\
//
// File set per task (from Roo Code's src/shared/globalFileNames.ts):
//
//	api_conversation_history.json   ← Anthropic MessageParam[] (what we ingest)
//	ui_messages.json                ← UI events with per-message ts
//	task_metadata.json
//	history_item.json
//
// Roo Code added a few message fields beyond stock Cline (reasoning_content,
// condenseId, truncation markers, etc.) — we tolerate them by extracting
// the standard `content` field and ignoring everything else.
//
// Note: Roo Code's main extension stream was discontinued mid-2026 in
// favor of forks like Zoo Code; existing user installs still have data
// at this path. This adapter covers them.
package roocode

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

const Source = "roocode"
const SessionPrefix = "roocode:"

// VS Code's extension ID for Roo Code (publisher "rooveterinaryinc",
// extension "roo-cline" — they kept the old slug after rebranding from
// "Roo Cline" to "Roo Code" so existing installs stay backward-compat).
const extensionDir = "rooveterinaryinc.roo-cline"

func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", extensionDir, "tasks")
	case "linux":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "Code", "User", "globalStorage", extensionDir, "tasks")
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, "Code", "User", "globalStorage", extensionDir, "tasks")
	}
	return filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", extensionDir, "tasks")
}

type IngestStats struct {
	RootExists  bool
	Tasks       int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

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
		return stats, fmt.Errorf("read roocode tasks dir: %w", err)
	}
	// Newest-first by taskId (which is Date.now() string).
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		convPath := filepath.Join(root, taskID, "api_conversation_history.json")
		if _, err := os.Stat(convPath); err != nil {
			continue
		}
		stats.Tasks++
		newS, newT, err := ingestTask(ctx, st, sourceID, taskID, convPath)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: roocode task %s: %v\n", taskID, err)
			continue
		}
		if newS {
			stats.NewSessions++
		}
		stats.NewTurns += newT
	}

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkSourceIngested(sourceID)
	}); err != nil {
		return stats, err
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

// apiMsg is the Roo-extended Anthropic MessageParam. Roo adds `ts` to
// every message (Unix ms), which means we get per-turn timestamps for
// free without parsing ui_messages.json — a nice upgrade over stock
// Cline. content can still be string or array-of-blocks.
type apiMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Ts      int64           `json:"ts,omitempty"`
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
	startedMs, _ := strconv.ParseInt(taskID, 10, 64) // Date.now()

	// Title: first non-empty user message, trimmed.
	title := ""
	for _, m := range msgs {
		if strings.ToLower(m.Role) != "user" {
			continue
		}
		if t := strings.TrimSpace(firstLine(extractText(m.Content))); t != "" {
			title = truncate(t, 200)
			break
		}
	}

	// Session ended_at = max ts across all turns (falls back to startedMs).
	endedMs := startedMs
	for _, m := range msgs {
		if m.Ts > endedMs {
			endedMs = m.Ts
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
		text := strings.TrimSpace(extractText(m.Content))
		if text == "" {
			continue
		}
		ts := m.Ts
		if ts == 0 {
			ts = startedMs
		}
		turnUUID := fmt.Sprintf("%s#%d", sessionID, i)
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

// extractText is the same flow as the Cline adapter — content can be
// string or array-of-blocks. We pull text blocks verbatim and summarize
// tool_use / tool_result so the FTS index stays focused.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text,omitempty"`
		Name    string          `json:"name,omitempty"`
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
			out = append(out, "[tool_result: "+truncate(string(b.Content), 120)+"]")
		case "image":
			out = append(out, "[image]")
		case "reasoning", "thinking":
			if b.Text != "" {
				out = append(out, "[reasoning] "+b.Text)
			}
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
