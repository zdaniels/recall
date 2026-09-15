// Package zed ingests Zed editor's native AI agent panel threads into
// recall.
//
// Zed persists every agent thread in a SQLite database at:
//
//	macOS:   ~/Library/Application Support/Zed/threads/threads.db
//	Linux:   ~/.local/share/zed/threads/threads.db   (XDG)
//	Windows: %LOCALAPPDATA%\Zed\threads\threads.db
//
// Schema (crates/agent/src/db.rs):
//
//	threads(id, summary, updated_at, created_at, parent_id,
//	        folder_paths, folder_paths_order, data_type, data BLOB)
//
// `data` is a zstd-compressed JSON serialization of DbThread, which
// contains `messages: Vec<Arc<DbMessage>>` and per-message timestamps.
// data_type is "Zstd" today; legacy rows may use "Json" or "Bincode"
// — we handle Zstd + Json, skip Bincode with a warning.
package zed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "zed"
const SessionPrefix = "zed:"

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Zed", "threads", "threads.db")
	case "linux":
		x := os.Getenv("XDG_DATA_HOME")
		if x == "" {
			x = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(x, "zed", "threads", "threads.db")
	case "windows":
		la := os.Getenv("LOCALAPPDATA")
		if la == "" {
			la = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(la, "Zed", "threads", "threads.db")
	}
	return filepath.Join(home, "Library", "Application Support", "Zed", "threads", "threads.db")
}

type IngestStats struct {
	DBExists    bool
	Threads     int
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
		return stats, fmt.Errorf("open zed db: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return stats, fmt.Errorf("ping zed db: %w", err)
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

	// Schema's been through ALTER TABLE migrations — be tolerant of
	// missing columns by COALESCE-ing what's optional.
	rows, err := src.QueryContext(ctx, `
		SELECT id,
		       COALESCE(summary, ''),
		       COALESCE(folder_paths, ''),
		       COALESCE(created_at, updated_at, ''),
		       COALESCE(updated_at, ''),
		       COALESCE(data_type, ''),
		       data
		  FROM threads
		 ORDER BY updated_at DESC`)
	if err != nil {
		return stats, fmt.Errorf("read zed threads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, summary, folder, created, updated, dtype string
			data                                         []byte
		)
		if err := rows.Scan(&id, &summary, &folder, &created, &updated, &dtype, &data); err != nil {
			stats.Errors++
			continue
		}
		stats.Threads++

		thread, err := decodeThread(dtype, data)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: zed thread %s (data_type=%s): %v\n", id, dtype, err)
			continue
		}

		sessionID := SessionPrefix + id
		startedMs := parseRFC3339Ms(created)
		endedMs := parseRFC3339Ms(updated)
		projectDir := firstFolderPath(folder)
		title := summary
		if title == "" {
			title = string(thread.Title)
		}

		var newSession bool
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			prev, err := tx.GetIngestState(sourceID, "thread:"+id)
			if err != nil {
				return err
			}
			if prev.LastOffset == 0 {
				newSession = true
			}
			return tx.UpsertSession(store.Session{
				ID:         sessionID,
				SourceID:   sourceID,
				ProjectDir: projectDir,
				StartedAt:  startedMs,
				EndedAt:    endedMs,
				Summary:    title,
			})
		}); err != nil {
			stats.Errors++
			continue
		}
		if newSession {
			stats.NewSessions++
		}

		// Replay only-new messages by tracking the last seen index.
		var lastIdx int64
		_ = st.Tx(ctx, func(tx *store.Tx) error {
			prev, err := tx.GetIngestState(sourceID, "thread:"+id)
			if err != nil {
				return err
			}
			lastIdx = prev.LastOffset
			return nil
		})

		newTurns := 0
		for i := int(lastIdx); i < len(thread.Messages); i++ {
			m := thread.Messages[i]
			text := strings.TrimSpace(m.text())
			if text == "" {
				continue
			}
			turnUUID := fmt.Sprintf("%s#%d", sessionID, i)
			ins, err := insertTurnAndIndex(ctx, st, store.Turn{
				UUID:      turnUUID,
				SessionID: sessionID,
				Idx:       i + 1,
				Role:      m.normalizedRole(),
				Ts:        m.tsMs(startedMs),
				Text:      text,
			})
			if err != nil {
				stats.Errors++
				continue
			}
			if ins {
				newTurns++
			}
		}
		stats.NewTurns += newTurns
		if int64(len(thread.Messages)) > lastIdx {
			_ = st.Tx(ctx, func(tx *store.Tx) error {
				return tx.SetIngestState(sourceID, "thread:"+id, store.IngestState{
					LastOffset: int64(len(thread.Messages)),
					LastLine:   len(thread.Messages),
					LastUUID:   fmt.Sprintf("%s#%d", sessionID, len(thread.Messages)-1),
				})
			})
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

// dbThread is the minimal shape we need from Zed's SerializedThread.
// Zed's full schema has a lot of optional decoration we don't care
// about — keeping this loose so version drift doesn't break ingest.
type dbThread struct {
	Title    string      `json:"title"`
	Messages []dbMessage `json:"messages"`
}

type dbMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// Zed has a few different per-message timestamp keys depending
	// on version; pick whichever's present.
	UpdatedAt string `json:"updated_at,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

func (m dbMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(m.Content, &arr); err == nil {
		var parts []string
		for _, b := range arr {
			switch {
			case b.Text != "":
				parts = append(parts, b.Text)
			case b.Name != "":
				parts = append(parts, "[tool: "+b.Name+"]")
			}
		}
		return strings.Join(parts, "\n")
	}
	// Fall back to the raw JSON so the search index isn't entirely empty.
	return string(m.Content)
}

func (m dbMessage) normalizedRole() string {
	switch strings.ToLower(m.Role) {
	case "user", "human":
		return "user"
	case "assistant", "model":
		return "assistant"
	case "system":
		return "system"
	case "tool":
		return "tool"
	default:
		return m.Role
	}
}

func (m dbMessage) tsMs(fallbackMs int64) int64 {
	for _, s := range []string{m.UpdatedAt, m.CreatedAt, m.Timestamp} {
		if ms := parseRFC3339Ms(s); ms > 0 {
			return ms
		}
	}
	return fallbackMs
}

// decodeThread reverses Zed's data_type-tagged storage. Zstd is the
// current default; Json is the older path. Bincode rows (oldest legacy)
// are not parseable without the Rust types — skip with an error.
func decodeThread(dataType string, data []byte) (*dbThread, error) {
	switch dataType {
	case "Zstd", "zstd", "":
		// Empty data_type defaults to Zstd per Zed v0.3.0
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		raw, err := dec.DecodeAll(data, nil)
		if err != nil {
			// Older rows might be raw JSON without compression — try it.
			if t, jerr := parseThreadJSON(data); jerr == nil {
				return t, nil
			}
			return nil, fmt.Errorf("zstd decompress: %w", err)
		}
		return parseThreadJSON(raw)
	case "Json", "json":
		return parseThreadJSON(data)
	case "Bincode", "bincode":
		return nil, fmt.Errorf("bincode rows are pre-v0.3.0 legacy and not supported (skip)")
	default:
		// Unknown — try JSON, then zstd, then give up.
		if t, err := parseThreadJSON(data); err == nil {
			return t, nil
		}
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		if raw, err := dec.DecodeAll(data, nil); err == nil {
			return parseThreadJSON(raw)
		}
		return nil, fmt.Errorf("unknown data_type %q", dataType)
	}
}

func parseThreadJSON(raw []byte) (*dbThread, error) {
	var t dbThread
	if err := json.Unmarshal(bytes.TrimSpace(raw), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// firstFolderPath extracts the first workspace directory from the
// comma-separated folder_paths column. Used as ProjectDir for scoping.
func firstFolderPath(joined string) string {
	if joined == "" {
		return ""
	}
	if i := strings.IndexByte(joined, ','); i > 0 {
		return strings.TrimSpace(joined[:i])
	}
	return strings.TrimSpace(joined)
}

// parseRFC3339Ms parses Zed's ISO-8601 timestamp strings into Unix ms.
// Returns 0 on failure so caller can detect "no timestamp available."
func parseRFC3339Ms(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
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
