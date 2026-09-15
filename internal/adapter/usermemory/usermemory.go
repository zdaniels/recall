// Package usermemory reads the user's curated Claude memory files into the
// decisions table.
//
// Layout: each project memory dir lives at
// `~/.claude/projects/<encoded-cwd>/memory/`, contains a MEMORY.md
// index plus typed entries (feedback_*.md, reference_*.md, etc) with
// YAML frontmatter:
//
//	---
//	name: short title
//	description: one-line summary used in MEMORY.md
//	type: feedback | reference | project | user
//	---
//
//	body...
//
// We treat each typed entry as a decision: text=description, kind=type,
// source='user-memory-md', salience=2.5 (above CLI 1.5 and surprise 2.0 —
// these are explicitly curated by the user). The MEMORY.md index itself
// is skipped.
//
// Idempotent: InsertDecisionIfNew dedups on (LOWER(text), project, source).
package usermemory

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/store"
)

const Source = "user-memory-md"
const baseSalience = 2.5

func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

type IngestStats struct {
	MemoryDirs   int
	Files        int
	NewDecisions int
	Errors       int
	Duration     time.Duration
}

func Ingest(ctx context.Context, st *store.Store, claudeRoot string) (IngestStats, error) {
	t0 := time.Now()
	var stats IngestStats

	if _, err := os.Stat(claudeRoot); err != nil {
		stats.Duration = time.Since(t0)
		return stats, nil
	}

	// Walk one level deep into projects/<encoded>/memory/
	entries, err := os.ReadDir(claudeRoot)
	if err != nil {
		return stats, err
	}

	var sourceID int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		id, err := tx.UpsertSource(Source, claudeRoot)
		if err != nil {
			return err
		}
		sourceID = id
		return nil
	}); err != nil {
		return stats, err
	}

	for _, projDir := range entries {
		if !projDir.IsDir() {
			continue
		}
		memDir := filepath.Join(claudeRoot, projDir.Name(), "memory")
		if _, err := os.Stat(memDir); err != nil {
			continue
		}
		stats.MemoryDirs++

		projectDir := DecodeProjectPath(projDir.Name())

		mdFiles, err := os.ReadDir(memDir)
		if err != nil {
			stats.Errors++
			continue
		}
		for _, f := range mdFiles {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(name, ".md") || name == "MEMORY.md" {
				continue
			}
			stats.Files++
			full := filepath.Join(memDir, name)
			fm, err := readFrontmatter(full)
			if err != nil {
				stats.Errors++
				fmt.Fprintf(os.Stderr, "warn: usermemory %s: %v\n", full, err)
				continue
			}
			text := strings.TrimSpace(fm.Description)
			if text == "" {
				continue
			}
			kind := strings.TrimSpace(fm.Type)
			if kind == "" {
				kind = "fact"
			}
			if err := st.Tx(ctx, func(tx *store.Tx) error {
				now := time.Now().Unix()
				id, ins, err := tx.InsertDecisionIfNew(store.Decision{
					ProjectDir: projectDir,
					Ts:         now,
					Kind:       kind,
					Text:       text,
					Source:     Source,
					Salience:   baseSalience,
				})
				if err != nil {
					return err
				}
				if ins {
					stats.NewDecisions++
					if err := entities.IndexInTx(tx, entities.KindDecision, fmt.Sprintf("%d", id), text, now); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				stats.Errors++
				fmt.Fprintf(os.Stderr, "warn: usermemory insert %s: %v\n", full, err)
			}
		}
	}

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkSourceIngested(sourceID)
	}); err != nil {
		return stats, err
	}
	_ = ignoreFS()
	stats.Duration = time.Since(t0)
	return stats, nil
}

func ignoreFS() error {
	var _ fs.DirEntry
	return nil
}

type frontmatter struct {
	Name        string
	Description string
	Type        string
}

// readFrontmatter scans up to the first 200 lines for a YAML-style
// `---\n…\n---\n` block. Tolerant of CRLF, indented values, and missing fields.
func readFrontmatter(path string) (frontmatter, error) {
	var fm frontmatter
	f, err := os.Open(path)
	if err != nil {
		return fm, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<14), 1<<20)

	if !sc.Scan() {
		return fm, sc.Err()
	}
	first := strings.TrimRight(sc.Text(), "\r")
	if first != "---" {
		return fm, nil // no frontmatter
	}
	for i := 0; i < 200 && sc.Scan(); i++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "---" {
			return fm, nil
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, `"`)
			switch strings.ToLower(key) {
			case "name":
				fm.Name = val
			case "description":
				fm.Description = val
			case "type":
				fm.Type = val
			}
		}
	}
	return fm, sc.Err()
}

// DecodeProjectPath best-effort-decodes a Claude-encoded project dir name
// (e.g. "-Users-z" -> "/Users/z"). For ambiguous encodings (paths with
// hyphens in their original name), we walk the path collapsing trailing
// segments back into hyphenated names until the result exists on disk.
func DecodeProjectPath(encoded string) string {
	if encoded == "" || encoded[0] != '-' {
		return ""
	}
	naive := strings.ReplaceAll(encoded, "-", "/")
	if _, err := os.Stat(naive); err == nil {
		return naive
	}
	// Try collapsing tail segments back into hyphenated names.
	parts := strings.Split(strings.TrimPrefix(naive, "/"), "/")
	for i := len(parts) - 1; i > 0; i-- {
		head := parts[:i]
		tail := strings.Join(parts[i:], "-")
		candidate := "/" + strings.Join(head, "/") + "/" + tail
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return naive
}
