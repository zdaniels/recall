// Package aider ingests Aider chat history files into recall.
//
// Aider writes a markdown log per project at:
//
//	<git-root>/.aider.chat.history.md
//
// Format (from aider/io.py: user_input() + ai_output()):
//
//	# aider chat started at 2026-05-27 10:43:00
//
//	#### user input line 1
//	#### user input line 2 (continuation lines also get the prefix)
//
//	assistant response text, possibly multi-line,
//	until the next ####-prefixed block.
//
//	#### second user prompt
//	...
//
// Each "# aider chat started at" line begins a new session; we treat
// the path + timestamp as the session id. User turns are the lines
// prefixed `#### `; everything between user blocks is the assistant
// response.
//
// The adapter scans a root directory (default: $HOME) up to a bounded
// depth for `.aider.chat.history.md` files. Hidden directories +
// large vendor dirs are skipped so the walk stays cheap.
package aider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "aider"
const SessionPrefix = "aider:"
const HistoryFile = ".aider.chat.history.md"

// DefaultRoot returns a single root for callers that want the simple
// shape. Equivalent to DefaultRoots()[0] when any preferred dev dir
// exists, otherwise $HOME. Kept for source compatibility with the
// single-root signature on cmd/recall ingest.
func DefaultRoot() string {
	roots := DefaultRoots()
	if len(roots) > 0 {
		return roots[0]
	}
	home, _ := os.UserHomeDir()
	return home
}

// DefaultRoots returns the list of directories to walk for Aider chat
// history files. We try a handful of common developer-tree locations
// in priority order; only directories that actually exist are
// returned. If none exist, the caller falls back to $HOME so users
// with code in unusual layouts still get covered (just slower).
func DefaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// Curated list — covers the vast majority of conventions:
	//   ~/code, ~/projects, ~/src, ~/dev, ~/work — common engineer roots
	//   ~/git, ~/repos                          — git-org-style layouts
	//   ~/workspace, ~/Workspace               — JetBrains / Eclipse default
	//   ~/Documents/code, ~/Documents/GitHub   — non-engineer / Windows-y
	candidates := []string{
		filepath.Join(home, "code"),
		filepath.Join(home, "projects"),
		filepath.Join(home, "src"),
		filepath.Join(home, "dev"),
		filepath.Join(home, "work"),
		filepath.Join(home, "git"),
		filepath.Join(home, "repos"),
		filepath.Join(home, "workspace"),
		filepath.Join(home, "Workspace"),
		filepath.Join(home, "Documents", "code"),
		filepath.Join(home, "Documents", "GitHub"),
	}
	var out []string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		// Nothing matched — fall back to $HOME so users with custom
		// layouts still get something. Slow on first walk; the
		// skip-dir guards keep it tolerable.
		out = []string{home}
	}
	return out
}

// IngestAll runs Ingest against each root in turn, accumulating stats.
// Caller-friendly wrapper for the multi-root common case.
func IngestAll(ctx context.Context, st *store.Store, roots []string) (IngestStats, error) {
	var combined IngestStats
	for _, r := range roots {
		s, err := Ingest(ctx, st, r)
		combined.RootExists = combined.RootExists || s.RootExists
		combined.Files += s.Files
		combined.Sessions += s.Sessions
		combined.NewSessions += s.NewSessions
		combined.NewTurns += s.NewTurns
		combined.Errors += s.Errors
		combined.Duration += s.Duration
		if err != nil {
			return combined, err
		}
	}
	return combined, nil
}

// MaxWalkDepth caps how deep into the filesystem the walker will go
// before giving up. Most coding projects sit within 2–4 levels of the
// home dir; 6 covers monorepo nesting without blowing through every
// node_modules + .cargo on disk.
const MaxWalkDepth = 6

type IngestStats struct {
	RootExists  bool
	Files       int
	Sessions    int
	NewSessions int
	NewTurns    int
	Errors      int
	Duration    time.Duration
}

// Ingest walks root looking for .aider.chat.history.md files and writes
// every chat session it finds into recall. Idempotent on re-run.
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

	files := []string{}
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate permission errors mid-walk
		}
		if d.IsDir() {
			depth := strings.Count(filepath.Clean(p), string(os.PathSeparator)) - rootDepth
			if depth >= MaxWalkDepth {
				return filepath.SkipDir
			}
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == HistoryFile {
			files = append(files, p)
		}
		return nil
	})

	for _, fp := range files {
		stats.Files++
		newS, newT, err := ingestFile(ctx, st, sourceID, fp)
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: aider %s: %v\n", fp, err)
			continue
		}
		stats.Sessions += newS // counts sessions found in this file
		stats.NewSessions += newS
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

// shouldSkipDir keeps the walk fast: never descend into common big +
// uninteresting subtrees that won't have an Aider history file.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".venv", "venv", "__pycache__",
		".cache", ".cargo", ".rustup", "target", "build", "dist",
		".npm", ".pnpm-store", ".yarn", ".gradle", ".m2", ".local",
		"Library", ".docker", ".kube", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "." && name != ".." && len(name) > 1
}

// ingestFile parses one .aider.chat.history.md, splits it into sessions,
// and writes each session + its turns into recall. Returns the number
// of new sessions and new turns observed in this file.
func ingestFile(ctx context.Context, st *store.Store, sourceID int64, path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	projectDir := filepath.Dir(path)
	projectHash := hashShort(projectDir)

	// Stream the file. The format is line-oriented; we never need to
	// hold more than the current session's user / assistant buffers in
	// memory at once.
	scanner := bufio.NewScanner(f)
	// Aider chats can contain long pasted code blocks → bump the line
	// buffer so we don't EOF on long single lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		newSessions int
		newTurns    int
		current     *sessionBuf
	)
	flush := func() {
		if current == nil {
			return
		}
		s, t, err := writeSession(ctx, st, sourceID, current)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: aider session in %s: %v\n", path, err)
			current = nil
			return
		}
		newSessions += s
		newTurns += t
		current = nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// New-session marker.
		if strings.HasPrefix(line, "# aider chat started at ") {
			flush()
			ts := strings.TrimPrefix(line, "# aider chat started at ")
			ts = strings.TrimSpace(ts)
			tsMs := parseTimestampMs(ts)
			sid := SessionPrefix + projectHash + "@" + ts
			current = &sessionBuf{
				ID:         sid,
				ProjectDir: projectDir,
				StartedMs:  tsMs,
				EndedMs:    tsMs,
			}
			continue
		}

		if current == nil {
			// Lines before any session marker — Aider does write a
			// banner before the first session sometimes. Skip.
			continue
		}

		if strings.HasPrefix(line, "#### ") {
			// User input line (possibly a continuation of the current
			// user turn — Aider prefixes every continuation line with
			// the same `#### `).
			text := strings.TrimPrefix(line, "#### ")
			if current.pendingAssistant.Len() > 0 {
				current.flushAssistant()
			}
			if current.pendingUser.Len() > 0 {
				current.pendingUser.WriteByte('\n')
			}
			current.pendingUser.WriteString(text)
			continue
		}

		// Anything else is assistant output. Flush any pending user
		// block first, then accumulate.
		if current.pendingUser.Len() > 0 {
			current.flushUser()
		}
		if current.pendingAssistant.Len() > 0 {
			current.pendingAssistant.WriteByte('\n')
		}
		current.pendingAssistant.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return newSessions, newTurns, fmt.Errorf("scan: %w", err)
	}
	flush()
	return newSessions, newTurns, nil
}

type sessionBuf struct {
	ID         string
	ProjectDir string
	StartedMs  int64
	EndedMs    int64

	turns            []store.Turn
	pendingUser      strings.Builder
	pendingAssistant strings.Builder
}

func (s *sessionBuf) flushUser() {
	text := strings.TrimSpace(s.pendingUser.String())
	s.pendingUser.Reset()
	if text == "" {
		return
	}
	s.turns = append(s.turns, store.Turn{
		Role: "user",
		Text: text,
	})
}

func (s *sessionBuf) flushAssistant() {
	text := strings.TrimSpace(s.pendingAssistant.String())
	s.pendingAssistant.Reset()
	if text == "" {
		return
	}
	s.turns = append(s.turns, store.Turn{
		Role: "assistant",
		Text: text,
	})
}

// writeSession commits one session and all its turns. Returns
// (newSession?, newTurnsCount, err).
func writeSession(ctx context.Context, st *store.Store, sourceID int64, s *sessionBuf) (int, int, error) {
	// Flush trailing pending buffers.
	if s.pendingUser.Len() > 0 {
		s.flushUser()
	}
	if s.pendingAssistant.Len() > 0 {
		s.flushAssistant()
	}
	if len(s.turns) == 0 {
		return 0, 0, nil
	}

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
			ID:         s.ID,
			SourceID:   sourceID,
			ProjectDir: s.ProjectDir,
			StartedAt:  s.StartedMs,
			EndedAt:    s.EndedMs,
		})
	}); err != nil {
		return 0, 0, err
	}

	newTurns := 0
	for i, t := range s.turns {
		t.SessionID = s.ID
		t.Idx = i + 1
		t.UUID = fmt.Sprintf("%s#%d", s.ID, i)
		t.Ts = s.StartedMs
		ins, err := insertTurnAndIndex(ctx, st, t)
		if err != nil {
			return boolToInt(newSession), newTurns, err
		}
		if ins {
			newTurns++
		}
	}

	_ = st.Tx(ctx, func(tx *store.Tx) error {
		return tx.SetIngestState(sourceID, "session:"+s.ID, store.IngestState{
			LastOffset: int64(len(s.turns)),
			LastLine:   len(s.turns),
			LastUUID:   fmt.Sprintf("%s#%d", s.ID, len(s.turns)-1),
		})
	})

	return boolToInt(newSession), newTurns, nil
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

// parseTimestampMs accepts Aider's documented timestamp formats and
// returns the Unix-ms value. Aider writes both YYYY-MM-DD HH:MM:SS
// (older versions) and RFC3339-ish; we try a couple of layouts and
// fall back to "now" if none match (the session still gets recorded,
// just with the ingest-time timestamp).
func parseTimestampMs(s string) int64 {
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UnixMilli()
		}
	}
	return time.Now().UnixMilli()
}

// hashShort returns the first 8 hex chars of sha1(s) — used to keep
// session ids short while still uniquely identifying the project dir.
func hashShort(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
