// Package goose ingests Block's Goose CLI agent sessions into recall.
//
// Goose persists every session in a SQLite database at:
//
//	macOS:   ~/Library/Application Support/Block/goose/sessions/sessions.db
//	Linux:   ~/.local/share/goose/sessions/sessions.db   (XDG)
//	Windows: %LOCALAPPDATA%\Block\goose\sessions\sessions.db
//
// Schema (from crates/goose/src/session/session_manager.rs):
//
//	sessions(id, name, description, working_dir, created_at, updated_at,
//	         session_type, total_tokens, ...)
//	messages(id, message_id, session_id, role, content_json,
//	         created_timestamp, timestamp, tokens, ...)
//
// content_json is a JSON-encoded MessageContent (the Goose Rust type),
// which can be a plain string or an array of content blocks. We pull
// text out of either shape and skip everything else (tool calls etc.
// are summarized for the FTS index).
//
// Read-only via SQLite ?mode=ro so we never block a running Goose.
package goose

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "goose"
const SessionPrefix = "goose:"

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db")
	case "linux":
		x := os.Getenv("XDG_DATA_HOME")
		if x == "" {
			x = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(x, "goose", "sessions", "sessions.db")
	case "windows":
		la := os.Getenv("LOCALAPPDATA")
		if la == "" {
			la = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(la, "Block", "goose", "sessions", "sessions.db")
	}
	return filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db")
}

type IngestStats struct {
	DBExists    bool
	Sessions    int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

func Ingest(ctx context.Context, st *store.Store, dbPath string) (IngestStats, error) {
	t0 := time.Now()
	var stats IngestStats

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			stats.Duration = time.Since(t0)
			return stats, nil
		}
		return stats, err
	}
	stats.DBExists = true

	src, err := sql.Open("sqlite", fmt.Sprintf(
		"file:%s?mode=ro&_pragma=busy_timeout(3000)", dbPath))
	if err != nil {
		return stats, fmt.Errorf("open goose db: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return stats, fmt.Errorf("ping goose db: %w", err)
	}

	var sourceID int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		id, err := tx.UpsertSource(Source, dbPath)
		if err != nil {
			return err
		}
		sourceID = id
		return nil
	}); err != nil {
		return stats, err
	}

	rows, err := src.QueryContext(ctx, `
		SELECT id,
		       COALESCE(name, ''),
		       COALESCE(working_dir, ''),
		       COALESCE(strftime('%s', created_at), '0'),
		       COALESCE(strftime('%s', updated_at), '0')
		  FROM sessions
		 ORDER BY updated_at DESC`)
	if err != nil {
		return stats, fmt.Errorf("read goose sessions: %w", err)
	}
	type sessRow struct {
		ID, Name, WorkDir string
		CreatedSec        int64
		UpdatedSec        int64
	}
	var allSessions []sessRow
	for rows.Next() {
		var r sessRow
		var c, u string
		if err := rows.Scan(&r.ID, &r.Name, &r.WorkDir, &c, &u); err != nil {
			stats.Errors++
			continue
		}
		fmt.Sscanf(c, "%d", &r.CreatedSec)
		fmt.Sscanf(u, "%d", &r.UpdatedSec)
		allSessions = append(allSessions, r)
	}
	rows.Close()

	for _, s := range allSessions {
		stats.Sessions++
		sessionID := SessionPrefix + s.ID

		var newSession bool
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			prev, err := tx.GetIngestState(sourceID, "session:"+s.ID)
			if err != nil {
				return err
			}
			if prev.LastOffset == 0 {
				newSession = true
			}
			return tx.UpsertSession(store.Session{
				ID:         sessionID,
				SourceID:   sourceID,
				ProjectDir: s.WorkDir,
				StartedAt:  s.CreatedSec * 1000,
				EndedAt:    s.UpdatedSec * 1000,
				Summary:    s.Name,
			})
		}); err != nil {
			stats.Errors++
			continue
		}
		if newSession {
			stats.NewSessions++
		}

		var lastIDSeen int64
		_ = st.Tx(ctx, func(tx *store.Tx) error {
			prev, err := tx.GetIngestState(sourceID, "session:"+s.ID)
			if err != nil {
				return err
			}
			lastIDSeen = prev.LastOffset
			return nil
		})

		msgs, err := src.QueryContext(ctx, `
			SELECT id, role, content_json, created_timestamp
			  FROM messages
			 WHERE session_id = ? AND id > ?
			 ORDER BY id ASC`, s.ID, lastIDSeen)
		if err != nil {
			stats.Errors++
			continue
		}

		newest := lastIDSeen
		idx := int(lastIDSeen)
		for msgs.Next() {
			var (
				mid       int64
				role      string
				contentJS string
				createdTs int64
			)
			if err := msgs.Scan(&mid, &role, &contentJS, &createdTs); err != nil {
				stats.Errors++
				continue
			}
			text := strings.TrimSpace(extractText(contentJS))
			if text == "" {
				newest = mid
				continue
			}
			idx++
			turnUUID := fmt.Sprintf("%s#%d", sessionID, mid)
			// created_timestamp is Unix ms in Goose; if it looks like
			// seconds (10-digit) we promote, else assume ms.
			tsMs := createdTs
			if createdTs > 0 && createdTs < 1_000_000_000_000 {
				tsMs = createdTs * 1000
			}
			ins, err := insertTurnAndIndex(ctx, st, store.Turn{
				UUID:      turnUUID,
				SessionID: sessionID,
				Idx:       idx,
				Role:      normalizeRole(role),
				Ts:        tsMs,
				Text:      text,
			})
			if err != nil {
				stats.Errors++
				continue
			}
			if ins {
				stats.NewTurns++
			}
			newest = mid
		}
		msgs.Close()

		if newest > lastIDSeen {
			_ = st.Tx(ctx, func(tx *store.Tx) error {
				return tx.SetIngestState(sourceID, "session:"+s.ID, store.IngestState{
					LastOffset: newest,
					LastLine:   idx,
					LastUUID:   fmt.Sprintf("%s#%d", sessionID, newest),
				})
			})
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

// extractText pulls human-readable text out of Goose's content_json.
// Goose's MessageContent is serde-tagged JSON: either a JSON string,
// a {"text": "..."} object, or an array of those (with content-block
// variants for tool calls/results). We pull text from each shape.
func extractText(raw string) string {
	if raw == "" {
		return ""
	}
	rb := []byte(raw)
	// Plain string?
	var s string
	if err := json.Unmarshal(rb, &s); err == nil {
		return s
	}
	// {"text": "..."} object?
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rb, &obj); err == nil {
		if t, ok := obj["text"]; ok {
			var v string
			if err := json.Unmarshal(t, &v); err == nil {
				return v
			}
		}
		if c, ok := obj["content"]; ok {
			return extractText(string(c))
		}
	}
	// Array of content blocks?
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(rb, &arr); err == nil {
		var parts []string
		for _, blk := range arr {
			if t, ok := blk["text"]; ok {
				var v string
				if err := json.Unmarshal(t, &v); err == nil && v != "" {
					parts = append(parts, v)
				}
			}
			// Surface tool-use blocks as short tags for searchability.
			if n, ok := blk["name"]; ok {
				var name string
				if err := json.Unmarshal(n, &name); err == nil && name != "" {
					parts = append(parts, "[tool: "+name+"]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
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
	case "tool":
		return "tool"
	default:
		return role
	}
}
