// Package hermes reads Hermes Agent's local SQLite state into recall.
//
// Hermes (NousResearch) persists every session at ~/.hermes/state.db.
// The schema is well-defined and stable (we read sessions + messages
// tables), and the database uses FTS5 + WAL — same architecture as
// recall itself, so ingestion is a clean schema mapping.
//
// Mapping:
//
//	sessions.id              -> sessions.id    (prefixed "hermes:")
//	sessions.title           -> sessions.summary
//	sessions.started_at      -> sessions.started_at  (REAL seconds → ms int64)
//	sessions.ended_at        -> sessions.ended_at
//	sessions.model           -> sessions.summary suffix (model tag)
//	messages.role/content    -> turns.role / turns.text
//	messages.timestamp       -> turns.ts
//
// We open the source DB read-only so we never block a running Hermes.
package hermes

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "hermes"

// SessionPrefix namespaces hermes session/turn ids against codex/claude/cursor
// so the recall store can't collide on what is otherwise a free-form TEXT id.
const SessionPrefix = "hermes:"

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hermes/state.db"
	}
	return filepath.Join(home, ".hermes", "state.db")
}

type IngestStats struct {
	DBExists    bool
	Sessions    int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

// Ingest walks every session + message in the Hermes DB and writes them
// into recall's store. Idempotent on re-run (UpsertSession + InsertTurn
// are both keyed by uuid). Missing DB is silent no-op.
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
		"file:%s?mode=ro&_pragma=busy_timeout(3000)",
		dbPath,
	))
	if err != nil {
		return stats, fmt.Errorf("open hermes db: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return stats, fmt.Errorf("ping hermes db: %w", err)
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

	// Pull sessions in ascending recency — we want to keep recent first
	// when the messages query joins. Hermes timestamps are REAL seconds
	// (Unix); we promote to ms-ints to match recall's store schema.
	rows, err := src.QueryContext(ctx, `
		SELECT id,
		       COALESCE(title, ''),
		       COALESCE(model, ''),
		       started_at,
		       COALESCE(ended_at, 0)
		  FROM sessions
		 ORDER BY started_at DESC`)
	if err != nil {
		return stats, fmt.Errorf("read sessions: %w", err)
	}
	sessionRows, err := drainSessions(rows)
	rows.Close()
	if err != nil {
		return stats, err
	}

	for _, s := range sessionRows {
		stats.Sessions++
		sessionID := SessionPrefix + s.ID
		startedMs := int64(s.StartedAtSec * 1000)
		endedMs := int64(s.EndedAtSec * 1000)

		// Compose a summary that includes the model tag when present so
		// future search hits surface "hermes / claude-opus-4.5" etc.
		summary := s.Title
		if s.Model != "" {
			if summary == "" {
				summary = s.Model
			} else {
				summary = summary + " (" + s.Model + ")"
			}
		}

		var sawNewSession bool
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			ingestKey := "session:" + s.ID
			prev, err := tx.GetIngestState(sourceID, ingestKey)
			if err != nil {
				return err
			}
			if prev.LastOffset == 0 {
				sawNewSession = true
			}
			return tx.UpsertSession(store.Session{
				ID:        sessionID,
				SourceID:  sourceID,
				StartedAt: startedMs,
				EndedAt:   endedMs,
				Summary:   summary,
			})
		}); err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: hermes session %s: %v\n", s.ID, err)
			continue
		}
		if sawNewSession {
			stats.NewSessions++
		}

		// Now pull messages for this session, ordered by timestamp.
		// We page by ingest_state.last_offset (= last seen messages.id)
		// so re-runs only grab new turns.
		var lastIDSeen int64
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			prev, err := tx.GetIngestState(sourceID, "session:"+s.ID)
			if err != nil {
				return err
			}
			lastIDSeen = prev.LastOffset
			return nil
		}); err != nil {
			stats.Errors++
			continue
		}

		msgRows, err := src.QueryContext(ctx, `
			SELECT id,
			       role,
			       COALESCE(content, ''),
			       timestamp,
			       COALESCE(token_count, 0)
			  FROM messages
			 WHERE session_id = ?
			   AND id > ?
			 ORDER BY id ASC`, s.ID, lastIDSeen)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: hermes messages for %s: %v\n", s.ID, err)
			continue
		}

		newest := lastIDSeen
		var idx int = int(lastIDSeen) // monotonic per-session turn index
		for msgRows.Next() {
			var (
				mid     int64
				role    string
				content string
				ts      float64
				tokens  int64
				_unused = tokens
			)
			_ = _unused
			if err := msgRows.Scan(&mid, &role, &content, &ts, &tokens); err != nil {
				stats.Errors++
				continue
			}
			content = strings.TrimSpace(content)
			if content == "" {
				newest = mid
				continue
			}
			idx++
			turnUUID := fmt.Sprintf("%s#%d", sessionID, mid)
			tsMs := int64(ts * 1000)
			ins, err := insertTurn(ctx, st, store.Turn{
				UUID:      turnUUID,
				SessionID: sessionID,
				Idx:       idx,
				Role:      normalizeRole(role),
				Ts:        tsMs,
				Text:      content,
			})
			if err != nil {
				stats.Errors++
				fmt.Fprintf(os.Stderr, "warn: hermes turn insert %d: %v\n", mid, err)
				continue
			}
			if ins {
				stats.NewTurns++
			}
			newest = mid
		}
		msgRows.Close()

		// Update high-water-mark so next ingest skips what we just wrote.
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

type sessionRow struct {
	ID           string
	Title        string
	Model        string
	StartedAtSec float64
	EndedAtSec   float64
}

func drainSessions(rows *sql.Rows) ([]sessionRow, error) {
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.ID, &r.Title, &r.Model, &r.StartedAtSec, &r.EndedAtSec); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// insertTurn wraps store.InsertTurn + entities.IndexInTx in one short
// transaction so the FTS index stays in sync with the turn row.
func insertTurn(ctx context.Context, st *store.Store, t store.Turn) (bool, error) {
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

// normalizeRole maps Hermes role strings (which include "tool" and
// "system") onto the recall canonical set. recall stores role as a free
// TEXT column, but normalizing here makes downstream filters predictable.
func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "user", "human":
		return "user"
	case "assistant", "model", "ai":
		return "assistant"
	case "system":
		return "system"
	case "tool", "tool_call", "tool_result":
		return "tool"
	default:
		return role
	}
}
