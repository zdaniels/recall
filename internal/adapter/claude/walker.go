package claude

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/decisions"
	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

// Source is the canonical kind for the sources row.
const Source = "claude-code"

// DefaultRoot returns ~/.claude/projects, which is where Claude Code stores
// session JSONL transcripts on macOS/Linux.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

func DiscoverSessions(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

type IngestStats struct {
	Files          int
	Sessions       int
	LinesProcessed int
	LinesSkipped   int
	NewTurns       int
	NewFiles       int
	NewDecisions   int
	Errors         int
	Duration       time.Duration
}

// Ingest walks all session JSONL files under root and writes them to the store.
// Idempotent: re-runs use ingest_state offsets to skip already-seen prefixes,
// and INSERT OR IGNORE on uuid handles any overlap.
func Ingest(ctx context.Context, st *store.Store, root string) (IngestStats, error) {
	t0 := time.Now()
	var stats IngestStats

	paths, err := DiscoverSessions(root)
	if err != nil {
		return stats, fmt.Errorf("discover: %w", err)
	}
	stats.Files = len(paths)

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

	for _, p := range paths {
		s, err := ingestOne(ctx, st, sourceID, p)
		stats.LinesProcessed += s.LinesProcessed
		stats.LinesSkipped += s.LinesSkipped
		stats.NewTurns += s.NewTurns
		stats.NewFiles += s.NewFiles
		stats.NewDecisions += s.NewDecisions
		if s.Touched {
			stats.Sessions++
		}
		if err != nil {
			stats.Errors++
			fmt.Fprintf(os.Stderr, "warn: ingest %s: %v\n", p, err)
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

type fileResult struct {
	LinesProcessed int
	LinesSkipped   int
	NewTurns       int
	NewFiles       int
	NewDecisions   int
	Touched        bool
}

func ingestOne(ctx context.Context, st *store.Store, sourceID int64, path string) (fileResult, error) {
	var res fileResult
	fi, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	var prev store.IngestState
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		s, err := tx.GetIngestState(sourceID, path)
		if err != nil {
			return err
		}
		prev = s
		return nil
	}); err != nil {
		return res, err
	}

	startOffset := prev.LastOffset
	if startOffset > fi.Size() {
		// file truncated or replaced — reread from start
		startOffset = 0
	}
	if startOffset == fi.Size() {
		// nothing new
		return res, nil
	}
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, 0); err != nil {
			return res, err
		}
	}

	// session-level fields are folded across the whole file
	var (
		idx              = prev.LastLine
		sessionAITitle   string
		sessionGitBranch string
		sessionVersion   string
		sessionStart     int64
		sessionEnd       int64
		sessionCWD       string

		turns        []store.Turn
		files        []store.File
		decs         []store.Decision
		lastUUIDSeen string
	)

	finalOffset, walkErr := LineReader(f, startOffset, func(offset int64, line []byte) error {
		ev, parseErr := ParseLine(line)
		if parseErr != nil {
			res.LinesSkipped++
			fmt.Fprintf(os.Stderr, "warn: %s offset=%d: parse: %v\n", path, offset, parseErr)
			return nil
		}
		if ev == nil {
			return nil
		}
		res.LinesProcessed++
		idx++

		if ev.GitBranch != "" {
			sessionGitBranch = ev.GitBranch
		}
		if ev.Version != "" {
			sessionVersion = ev.Version
		}
		if ev.CWD != "" {
			sessionCWD = ev.CWD
		}
		if !ev.Timestamp.IsZero() {
			ts := ev.Timestamp.Unix()
			if sessionStart == 0 || ts < sessionStart {
				sessionStart = ts
			}
			if ts > sessionEnd {
				sessionEnd = ts
			}
		}

		switch ev.Type {
		case "ai-title":
			if ev.AITitle != "" {
				sessionAITitle = ev.AITitle
			}
		case "user", "assistant":
			role, blocks, err := ev.DecodeMessage()
			if err != nil {
				res.LinesSkipped++
				fmt.Fprintf(os.Stderr, "warn: %s line %d: decode message: %v\n", path, idx, err)
				return nil
			}
			if role == "" {
				role = ev.Type
			}
			tools := ExtractToolUses(blocks)
			turn := store.Turn{
				UUID:       ev.UUID,
				SessionID:  sessionID,
				ParentUUID: ev.ParentUUID,
				Idx:        idx,
				Role:       role,
				Ts:         maybeUnix(ev.Timestamp),
				Text:       ExtractText(blocks),
				HasToolUse: len(tools) > 0,
			}
			if turn.UUID == "" {
				// guarantee PK uniqueness even on legacy events lacking uuid
				turn.UUID = fmt.Sprintf("%s#%d", sessionID, idx)
			}
			turns = append(turns, turn)
			lastUUIDSeen = turn.UUID

			for _, b := range tools {
				for _, fop := range FilesFromToolUse(b) {
					files = append(files, store.File{
						SessionID:  sessionID,
						TurnUUID:   turn.UUID,
						ProjectDir: ev.CWD,
						Path:       fop.Path,
						Op:         fop.Op,
						Ts:         maybeUnix(ev.Timestamp),
					})
				}
			}

			// Pattern-detected decisions: only on user turns. Low-salience candidates.
			if role == "user" && turn.Text != "" {
				for _, c := range decisions.Match(turn.Text) {
					decs = append(decs, store.Decision{
						ProjectDir: ev.CWD,
						SessionID:  sessionID,
						Ts:         maybeUnix(ev.Timestamp),
						Kind:       c.Kind,
						Text:       c.Text,
						Source:     "pattern",
						Salience:   0.5,
					})
				}
			}
		default:
			// schema-drift tolerance: ignore unknown event types
			res.LinesSkipped++
		}
		return nil
	})

	flushErr := st.Tx(ctx, func(tx *store.Tx) error {
		sess := store.Session{
			ID:            sessionID,
			SourceID:      sourceID,
			ProjectDir:    sessionCWD,
			GitBranch:     sessionGitBranch,
			SourceVersion: sessionVersion,
			StartedAt:     sessionStart,
			EndedAt:       sessionEnd,
			Summary:       sessionAITitle,
		}
		if err := tx.UpsertSession(sess); err != nil {
			return err
		}
		res.Touched = true
		for _, t := range turns {
			ins, err := tx.InsertTurn(t)
			if err != nil {
				return err
			}
			if ins {
				res.NewTurns++
				if err := entities.IndexInTx(tx, entities.KindTurn, t.UUID, t.Text, t.Ts); err != nil {
					return err
				}
			}
		}
		for _, f := range files {
			ins, err := tx.InsertFile(f)
			if err != nil {
				return err
			}
			if ins {
				res.NewFiles++
			}
		}
		for _, d := range decs {
			id, ins, err := tx.InsertDecisionIfNew(d)
			if err != nil {
				return err
			}
			if ins {
				res.NewDecisions++
				if err := entities.IndexInTx(tx, entities.KindDecision, fmt.Sprintf("%d", id), d.Text, d.Ts); err != nil {
					return err
				}
			}
		}
		// Populate co-occurrence edges so spreading-activation recall has
		// data to walk later. file↔session links every touched file to this
		// session; file↔file with canonical ordering captures pairs that
		// co-occurred. We dedupe per-session before writing — same file
		// touched many times in a session counts once for edge weight.
		seenFiles := map[string]struct{}{}
		for _, f := range files {
			seenFiles[f.Path] = struct{}{}
		}
		uniq := make([]string, 0, len(seenFiles))
		for p := range seenFiles {
			uniq = append(uniq, p)
		}
		for _, p := range uniq {
			_ = tx.UpsertEdge("file", p, "session", sessionID)
		}
		// pairwise edges (canonical lex order to avoid double-counting)
		for i := 0; i < len(uniq); i++ {
			for j := i + 1; j < len(uniq); j++ {
				a, b := uniq[i], uniq[j]
				if a > b {
					a, b = b, a
				}
				_ = tx.UpsertEdge("file", a, "file", b)
			}
		}
		return tx.SetIngestState(sourceID, path, store.IngestState{
			LastOffset: finalOffset,
			LastLine:   idx,
			LastUUID:   lastUUIDSeen,
		})
	})

	if walkErr != nil {
		return res, walkErr
	}
	return res, flushErr
}

func maybeUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
