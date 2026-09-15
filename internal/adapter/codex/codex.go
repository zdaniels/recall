// Package codex reads the Codex CLI's SQLite state into recall.
//
// Codex stores threads in ~/.codex/state_5.sqlite. Each thread row carries
// useful session metadata (cwd, title, first_user_message, git_branch,
// timestamps). Per-turn conversation lives in separate "rollout" files
// referenced by threads.rollout_path; we do NOT parse those yet — there
// were no samples on the build host. Future cuts can add a rollout
// parser without changing the schema.
//
// Mapping:
//
//	thread.id                  -> sessions.id     (prefixed "codex:" to namespace)
//	thread.cwd                 -> sessions.project_dir
//	thread.title               -> sessions.summary
//	thread.git_branch          -> sessions.git_branch
//	thread.cli_version         -> sessions.source_version
//	thread.created_at          -> sessions.started_at
//	thread.updated_at          -> sessions.ended_at
//	thread.first_user_message  -> synthetic turn (idx=1, role=user)
//
// We open the source DB read-only (mode=ro) so we never block Codex.
package codex

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

const Source = "codex"

// SessionPrefix namespaces codex-derived session and turn ids so they can't
// collide with other adapters' uuids.
const SessionPrefix = "codex:"

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex/state_5.sqlite"
	}
	return filepath.Join(home, ".codex", "state_5.sqlite")
}

type IngestStats struct {
	DBExists    bool
	Threads     int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

// Ingest walks the codex state DB and writes threads as sessions into st.
// Idempotent: InsertTurn UUID-keyed, UpsertSession merges fields.
// If the codex DB is missing, returns DBExists=false and a nil error
// (treat as silent no-op for users without codex installed).
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
		return stats, fmt.Errorf("open codex db: %w", err)
	}
	defer src.Close()

	if err := src.PingContext(ctx); err != nil {
		return stats, fmt.Errorf("ping codex db: %w", err)
	}

	// upsert source row
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

	// schema-tolerant query — only the columns we actually need.
	rows, err := src.QueryContext(ctx, `
		SELECT id,
		       COALESCE(cwd, ''),
		       COALESCE(title, ''),
		       COALESCE(first_user_message, ''),
		       COALESCE(git_branch, ''),
		       COALESCE(cli_version, ''),
		       COALESCE(created_at, 0),
		       COALESCE(updated_at, 0)
		  FROM threads
		 WHERE COALESCE(archived, 0) = 0
		 ORDER BY updated_at DESC`)
	if err != nil {
		// schema drift: codex changed the threads table. Don't crash; warn and bail.
		return stats, fmt.Errorf("read threads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, cwd, title, firstMsg, branch, ver string
			createdAt, updatedAt                  int64
		)
		if err := rows.Scan(&id, &cwd, &title, &firstMsg, &branch, &ver, &createdAt, &updatedAt); err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: codex scan: %v\n", err)
			continue
		}
		stats.Threads++

		sessionID := SessionPrefix + id

		var newSession, newTurn bool
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			// detect first-time insert by checking ingest_state per-thread key
			ingestKey := "thread:" + id
			prev, err := tx.GetIngestState(sourceID, ingestKey)
			if err != nil {
				return err
			}
			if prev.LastOffset == 0 {
				newSession = true
			}

			if err := tx.UpsertSession(store.Session{
				ID:            sessionID,
				SourceID:      sourceID,
				ProjectDir:    cwd,
				GitBranch:     branch,
				SourceVersion: ver,
				StartedAt:     createdAt,
				EndedAt:       updatedAt,
				Summary:       title,
			}); err != nil {
				return err
			}

			if firstMsg = strings.TrimSpace(firstMsg); firstMsg != "" {
				turnUUID := sessionID + "#first"
				ins, err := tx.InsertTurn(store.Turn{
					UUID:      turnUUID,
					SessionID: sessionID,
					Idx:       1,
					Role:      "user",
					Ts:        createdAt,
					Text:      firstMsg,
				})
				if err != nil {
					return err
				}
				newTurn = ins
				if ins {
					if err := entities.IndexInTx(tx, entities.KindTurn, turnUUID, firstMsg, createdAt); err != nil {
						return err
					}
				}
			}

			// Use ingest_state.last_offset as the high-water-mark of updated_at_ms-equiv.
			// Re-runs hit this and skip threads we've already fully processed.
			return tx.SetIngestState(sourceID, ingestKey, store.IngestState{
				LastOffset: updatedAt,
				LastLine:   1,
				LastUUID:   sessionID + "#first",
			})
		}); err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: codex thread %s: %v\n", id, err)
			continue
		}
		if newSession {
			stats.NewSessions++
		}
		if newTurn {
			stats.NewTurns++
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkSourceIngested(sourceID)
	}); err != nil {
		return stats, err
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}
