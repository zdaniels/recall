// Package continueide ingests Continue.dev session history into recall.
//
// Continue is a VS Code / JetBrains extension that persists every chat
// session as a JSON file under the user's global Continue dir:
//
//	~/.continue/sessions/<sessionId>.json   ← full Session
//	~/.continue/sessions/sessions.json      ← index (BaseSessionMetadata[])
//
// The index gives us dateCreated and title without parsing every full
// session; we still read the per-session file to get the chat history.
//
// Session shape (continue/core/index.d.ts):
//
//	Session {
//	  sessionId, title, workspaceDirectory, history: ChatHistoryItem[],
//	  mode?, chatModelTitle?, usage?
//	}
//	ChatHistoryItem { message: ChatMessage, ...metadata }
//	ChatMessage     { role, content }  // content is string or message-parts[]
//
// Continue stores no per-message timestamps, so every turn inherits the
// session's dateCreated. ended_at is set to dateCreated too (refine
// once Continue starts persisting per-message ts upstream).
//
// Package name `continueide` because `continue` is a Go keyword.
package continueide

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "continue"
const SessionPrefix = "continue:"

// DefaultRoot returns ~/.continue/sessions on every platform Continue
// targets. Continue uses the same path on macOS, Linux, and Windows
// (just $HOME on each).
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".continue", "sessions")
}

type IngestStats struct {
	RootExists  bool
	Sessions    int
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

	// Read the sessions.json index for dateCreated + title up front.
	// If it's missing or malformed, fall back to per-file walks.
	metaByID := loadIndex(root)

	entries, err := os.ReadDir(root)
	if err != nil {
		return stats, fmt.Errorf("read continue sessions dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "sessions.json" {
			continue
		}
		stats.Sessions++
		sessionID := strings.TrimSuffix(e.Name(), ".json")
		newS, newT, err := ingestSession(ctx, st, sourceID, root, sessionID, metaByID[sessionID])
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: continue %s: %v\n", sessionID, err)
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

// indexEntry mirrors Continue's BaseSessionMetadata — every field is
// optional from our side because old sessions.json files have varied
// shapes.
type indexEntry struct {
	SessionID          string `json:"sessionId"`
	Title              string `json:"title"`
	DateCreated        string `json:"dateCreated"` // String of Date.now()
	WorkspaceDirectory string `json:"workspaceDirectory"`
}

func loadIndex(root string) map[string]indexEntry {
	raw, err := os.ReadFile(filepath.Join(root, "sessions.json"))
	if err != nil {
		return nil
	}
	var entries []indexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make(map[string]indexEntry, len(entries))
	for _, e := range entries {
		if e.SessionID != "" {
			out[e.SessionID] = e
		}
	}
	return out
}

// session is the relevant subset of Continue's Session interface.
type session struct {
	SessionID          string       `json:"sessionId"`
	Title              string       `json:"title"`
	WorkspaceDirectory string       `json:"workspaceDirectory"`
	History            []historyRow `json:"history"`
}

type historyRow struct {
	Message message `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func ingestSession(ctx context.Context, st *store.Store, sourceID int64, root, sessionIDFromFile string, meta indexEntry) (bool, int, error) {
	raw, err := os.ReadFile(filepath.Join(root, sessionIDFromFile+".json"))
	if err != nil {
		return false, 0, err
	}
	var s session
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, 0, fmt.Errorf("parse session: %w", err)
	}

	// Resolve the canonical sessionId — prefer the embedded one, fall
	// back to the filename if the JSON omits it.
	id := s.SessionID
	if id == "" {
		id = sessionIDFromFile
	}
	sessionID := SessionPrefix + id

	// Timestamps: try the index first; fall back to file mtime.
	var startedMs int64
	if meta.DateCreated != "" {
		if ms, err := strconv.ParseInt(meta.DateCreated, 10, 64); err == nil {
			startedMs = ms
		}
	}
	if startedMs == 0 {
		if fi, err := os.Stat(filepath.Join(root, sessionIDFromFile+".json")); err == nil {
			startedMs = fi.ModTime().UnixMilli()
		}
	}

	title := s.Title
	if title == "" {
		title = meta.Title
	}
	workspace := s.WorkspaceDirectory
	if workspace == "" {
		workspace = meta.WorkspaceDirectory
	}

	var newSession bool
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		prev, err := tx.GetIngestState(sourceID, "session:"+id)
		if err != nil {
			return err
		}
		if prev.LastOffset == 0 {
			newSession = true
		}
		return tx.UpsertSession(store.Session{
			ID:         sessionID,
			SourceID:   sourceID,
			ProjectDir: workspace,
			StartedAt:  startedMs,
			EndedAt:    startedMs,
			Summary:    title,
		})
	}); err != nil {
		return false, 0, err
	}

	// Replay turns from a resumption checkpoint so re-runs are cheap.
	var lastIdx int64
	_ = st.Tx(ctx, func(tx *store.Tx) error {
		prev, err := tx.GetIngestState(sourceID, "session:"+id)
		if err != nil {
			return err
		}
		lastIdx = prev.LastOffset
		return nil
	})

	newTurns := 0
	for i := int(lastIdx); i < len(s.History); i++ {
		text := strings.TrimSpace(extractText(s.History[i].Message.Content))
		if text == "" {
			continue
		}
		turnUUID := fmt.Sprintf("%s#%d", sessionID, i)
		ins, err := insertTurnAndIndex(ctx, st, store.Turn{
			UUID:      turnUUID,
			SessionID: sessionID,
			Idx:       i + 1,
			Role:      normalizeRole(s.History[i].Message.Role),
			Ts:        startedMs,
			Text:      text,
		})
		if err != nil {
			return newSession, newTurns, err
		}
		if ins {
			newTurns++
		}
	}

	if int64(len(s.History)) > lastIdx {
		_ = st.Tx(ctx, func(tx *store.Tx) error {
			return tx.SetIngestState(sourceID, "session:"+id, store.IngestState{
				LastOffset: int64(len(s.History)),
				LastLine:   len(s.History),
				LastUUID:   fmt.Sprintf("%s#%d", sessionID, len(s.History)-1),
			})
		})
	}
	return newSession, newTurns, nil
}

// extractText pulls text out of a Continue ChatMessage's `content` —
// which can be either a JSON string OR an array of message-part blocks
// (similar to Anthropic's MessageParam). Tool-related blocks summarize
// to keep the FTS index focused on conversational content.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
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
	case "thinking":
		return "assistant" // surface thinking content under assistant
	case "tool":
		return "tool"
	default:
		return role
	}
}
