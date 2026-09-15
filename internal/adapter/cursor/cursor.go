// Package cursor reads Cursor's chat history into recall.
//
// Cursor (a VS Code fork) stores composer (chat) sessions in its
// globalStorage state.vscdb, located per-OS like any VS Code variant:
//
//	macOS:   ~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
//	Linux:   ~/.config/Cursor/User/globalStorage/state.vscdb  (honors XDG_CONFIG_HOME)
//	Windows: %APPDATA%/Cursor/User/globalStorage/state.vscdb
//
// The DB has two tables of interest:
//
//   - ItemTable['composer.composerHeaders']     — JSON index of all composers
//   - cursorDiskKV['bubbleId:<cid>:<bid>']      — one row per turn, JSON value
//   - cursorDiskKV['composerData:<cid>']        — composer metadata (unused)
//
// Mapping:
//
//	composer.composerId       -> sessions.id ("cursor:<composerId>")
//	composer.name|subtitle    -> sessions.summary
//	composer.createdAt (ms)   -> sessions.started_at (seconds)
//	composer.lastUpdatedAt    -> sessions.ended_at
//	bubble.bubbleId           -> turns.uuid ("cursor:<composerId>:<bubbleId>")
//	bubble.type (1=user|2=assistant) -> turns.role
//	bubble.text               -> turns.text
//	bubble.createdAt (ISO8601)-> turns.ts
//
// All ids are namespaced "cursor:" so they can't collide with other adapters.
package cursor

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

const Source = "cursor"
const SessionPrefix = "cursor:"

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	}
	const tail = "state.vscdb" // under <base>/Cursor/User/globalStorage/
	switch runtime.GOOS {
	case "linux":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "Cursor", "User", "globalStorage", tail)
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appdata, "Cursor", "User", "globalStorage", tail)
	default: // darwin + fallback
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", tail)
	}
}

type IngestStats struct {
	DBExists    bool
	Composers   int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

// bubble matches the subset of fields we read from cursorDiskKV bubble blobs.
// Cursor bubbles include rich-text trees + metadata we don't care about; we
// only pull the fields needed to round-trip into turns. Unknown fields are
// ignored so we tolerate Cursor schema drift.
type bubble struct {
	BubbleID  string `json:"bubbleId"`
	Type      int    `json:"type"` // 1 = user, 2 = assistant
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"` // ISO8601
}

// composerHeader matches the subset of fields we read from composer.composerHeaders.
// Unknown fields are ignored — this is schema-drift tolerant.
type composerHeader struct {
	ComposerID          string `json:"composerId"`
	Name                string `json:"name"`
	Subtitle            string `json:"subtitle"`
	CreatedAt           int64  `json:"createdAt"`     // ms
	LastUpdatedAt       int64  `json:"lastUpdatedAt"` // ms
	IsArchived          bool   `json:"isArchived"`
	WorkspaceIdentifier struct {
		ID string `json:"id"`
	} `json:"workspaceIdentifier"`
}

type composerHeadersBlob struct {
	AllComposers []composerHeader `json:"allComposers"`
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
		"file:%s?mode=ro&_pragma=busy_timeout(3000)",
		dbPath,
	))
	if err != nil {
		return stats, fmt.Errorf("open cursor db: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return stats, fmt.Errorf("ping cursor db: %w", err)
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

	var headersJSON string
	err = src.QueryRowContext(ctx,
		`SELECT value FROM ItemTable WHERE key = 'composer.composerHeaders'`,
	).Scan(&headersJSON)
	if err == sql.ErrNoRows {
		// no chat history yet
		stats.Duration = time.Since(t0)
		return stats, nil
	}
	if err != nil {
		return stats, fmt.Errorf("read composerHeaders: %w", err)
	}

	var blob composerHeadersBlob
	if err := json.Unmarshal([]byte(headersJSON), &blob); err != nil {
		return stats, fmt.Errorf("parse composerHeaders: %w", err)
	}

	for _, c := range blob.AllComposers {
		if c.IsArchived || c.ComposerID == "" {
			continue
		}
		stats.Composers++

		summary := strings.TrimSpace(c.Name)
		if summary == "" {
			summary = strings.TrimSpace(c.Subtitle)
		}
		if summary == "" {
			// composers with no name and no subtitle are usually empty drafts — skip
			continue
		}

		sessionID := SessionPrefix + c.ComposerID
		started := msToSec(c.CreatedAt)
		ended := msToSec(c.LastUpdatedAt)
		if ended == 0 {
			ended = started
		}

		bubbles, berr := loadBubbles(ctx, src, c.ComposerID)
		if berr != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: cursor bubbles %s: %v\n", c.ComposerID, berr)
			// fall through and still upsert the session header
		}

		var newSession bool
		var newTurns int
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			ingestKey := "composer:" + c.ComposerID
			prev, err := tx.GetIngestState(sourceID, ingestKey)
			if err != nil {
				return err
			}
			if prev.LastOffset == 0 {
				newSession = true
			}
			if err := tx.UpsertSession(store.Session{
				ID:        sessionID,
				SourceID:  sourceID,
				StartedAt: started,
				EndedAt:   ended,
				Summary:   summary,
				// project_dir intentionally empty — workspaceIdentifier.id
				// is opaque without a separate workspace→folder lookup; skip
				// rather than emit garbage. Workspace mapping is a v1.1 task.
			}); err != nil {
				return err
			}
			for i, b := range bubbles {
				turnUUID := sessionID + ":" + b.BubbleID
				ts := parseISO8601(b.CreatedAt)
				inserted, err := tx.InsertTurn(store.Turn{
					UUID:      turnUUID,
					SessionID: sessionID,
					Idx:       i,
					Role:      bubbleRole(b.Type),
					Ts:        ts,
					Text:      b.Text,
				})
				if err != nil {
					return err
				}
				if inserted {
					newTurns++
					if err := entities.IndexInTx(tx, entities.KindTurn, turnUUID, b.Text, ts); err != nil {
						return err
					}
				}
			}
			return tx.SetIngestState(sourceID, ingestKey, store.IngestState{
				LastOffset: ended,
				LastLine:   len(bubbles),
			})
		}); err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: cursor composer %s: %v\n", c.ComposerID, err)
			continue
		}
		if newSession {
			stats.NewSessions++
		}
		stats.NewTurns += newTurns
	}

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkSourceIngested(sourceID)
	}); err != nil {
		return stats, err
	}
	stats.Duration = time.Since(t0)
	return stats, nil
}

func msToSec(ms int64) int64 {
	if ms == 0 {
		return 0
	}
	return ms / 1000
}

// loadBubbles reads all bubbleId:<composerID>:* rows for one composer,
// parses their JSON, drops empties, and returns them sorted by createdAt
// so InsertTurn gets stable monotonic idx values.
func loadBubbles(ctx context.Context, src *sql.DB, composerID string) ([]bubble, error) {
	rows, err := src.QueryContext(ctx,
		`SELECT value FROM cursorDiskKV WHERE key LIKE ?`,
		"bubbleId:"+composerID+":%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bubble
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var b bubble
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			// One bad bubble shouldn't kill the whole composer — skip and continue.
			continue
		}
		if strings.TrimSpace(b.Text) == "" || b.BubbleID == "" {
			continue
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	sortBubbles(out)
	return out, nil
}

func sortBubbles(b []bubble) {
	// Bubble counts per composer are small (tens, not thousands); in-place
	// insertion sort keeps the dependency footprint tiny.
	for i := 1; i < len(b); i++ {
		j := i
		for j > 0 && bubbleLess(b[j], b[j-1]) {
			b[j], b[j-1] = b[j-1], b[j]
			j--
		}
	}
}

func bubbleLess(a, b bubble) bool {
	at := parseISO8601(a.CreatedAt)
	bt := parseISO8601(b.CreatedAt)
	if at != bt {
		return at < bt
	}
	return a.BubbleID < b.BubbleID
}

func bubbleRole(t int) string {
	switch t {
	case 1:
		return "user"
	case 2:
		return "assistant"
	default:
		return "unknown"
	}
}

func parseISO8601(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	return t.Unix()
}
