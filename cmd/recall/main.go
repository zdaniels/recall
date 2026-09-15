// Command recall is the recall CLI — local cross-tool memory for coding agents.
//
// M0 surface: ingest (read source agent transcripts into the DB) + stats.
// More commands (search, files, sessions, doctor, mcp) land in M1+.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zdaniels/recall/internal/adapter/aider"
	"github.com/zdaniels/recall/internal/adapter/claude"
	"github.com/zdaniels/recall/internal/adapter/cline"
	"github.com/zdaniels/recall/internal/adapter/codex"
	continueide "github.com/zdaniels/recall/internal/adapter/continueide"
	"github.com/zdaniels/recall/internal/adapter/cursor"
	"github.com/zdaniels/recall/internal/adapter/goose"
	"github.com/zdaniels/recall/internal/adapter/hermes"
	"github.com/zdaniels/recall/internal/adapter/kilocode"
	"github.com/zdaniels/recall/internal/adapter/roocode"
	"github.com/zdaniels/recall/internal/adapter/usermemory"
	"github.com/zdaniels/recall/internal/adapter/zed"
	"github.com/zdaniels/recall/internal/bench"
	"github.com/zdaniels/recall/internal/consolidate"
	"github.com/zdaniels/recall/internal/distill"
	"github.com/zdaniels/recall/internal/doctor"
	"github.com/zdaniels/recall/internal/embed"
	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/inject"
	"github.com/zdaniels/recall/internal/maintain"
	mcpserver "github.com/zdaniels/recall/internal/mcp"
	"github.com/zdaniels/recall/internal/recall"
	"github.com/zdaniels/recall/internal/reconsolidate"
	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
	"github.com/zdaniels/recall/internal/wiki"
)

var version = "0.1.0"

func defaultDBPath() string {
	// RECALL_DB is the canonical override; AGENT_MEMORY_DB is the legacy
	// env name from the pre-rename era and is honored for one release so
	// existing scripts don't break overnight.
	if v := os.Getenv("RECALL_DB"); v != "" {
		return v
	}
	if v := os.Getenv("AGENT_MEMORY_DB"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	newDir := filepath.Join(home, ".local", "share", "recall")
	newPath := filepath.Join(newDir, "db.sqlite")
	// One-shot migration: existing installs put the DB at
	// ~/.local/share/agent-memory/. If the new dir doesn't exist yet but
	// the old one does, rename in place. Idempotent: subsequent calls
	// just hit the new dir and return.
	if _, err := os.Stat(newDir); os.IsNotExist(err) {
		oldDir := filepath.Join(home, ".local", "share", "agent-memory")
		if _, err := os.Stat(oldDir); err == nil {
			_ = os.Rename(oldDir, newDir)
		}
	}
	return newPath
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx := context.Background()
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "ingest":
		err = cmdIngest(ctx, args)
	case "inject":
		err = cmdInject(ctx, args)
	case "stats":
		err = cmdStats(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "decide":
		err = cmdDecide(ctx, args)
	case "decisions":
		err = cmdDecisions(ctx, args)
	case "forget":
		err = cmdForget(ctx, args)
	case "record-tool-event":
		err = cmdRecordToolEvent(ctx, args)
	case "search":
		err = cmdSearch(ctx, args)
	case "embed":
		err = cmdEmbed(ctx, args)
	case "paraphrase":
		err = cmdParaphrase(ctx, args)
	case "download-model":
		err = cmdDownloadModel(ctx, args)
	case "install-model":
		err = cmdInstallModel(ctx, args)
	case "consolidate":
		err = cmdConsolidate(ctx, args)
	case "pin":
		err = cmdPin(ctx, args)
	case "unpin":
		err = cmdUnpin(ctx, args)
	case "pins":
		err = cmdPins(ctx, args)
	case "bench":
		err = cmdBench(ctx, args)
	case "distill":
		err = cmdDistill(ctx, args)
	case "maintain":
		err = cmdMaintain(ctx, args)
	case "wiki":
		err = cmdWiki(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("recall", version)
	case "help", "--help", "-h":
		usage(os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, `recall — local cross-tool memory CLI

usage:
  recall ingest [--source claude-code] [--root PATH] [--db PATH]
  recall inject [--project DIR] [--days N] [--budget N] [--db PATH]
  recall stats  [--db PATH]
  recall mcp    [--db PATH]                  # stdio MCP server
  recall doctor [--db PATH]                  # health check
  recall watch  [--interval 30s] [--db PATH] # poll all sources on a timer
  recall decide TEXT... [--kind fact|preference|feedback] [--project DIR|-] [--salience N]
  recall decisions      [--project DIR|all] [--kind K] [--limit N] [--json]
  recall forget         (--decision ID | --pattern-text TEXT)
  recall record-tool-event                   # reads PostToolUse JSON from stdin
  recall search QUERY [--project DIR|all] [--limit N] [--json]
  recall download-model [--force]            # fetch ONNX runtime + sentence-transformer (sha256-verified)
  recall install-model  --from DIR           # side-load assets from a local dir (airgap / mirror)
  recall embed [--provider onnx|apple|ollama] [--scope decisions|turns] [--batch N]
  recall consolidate [--days N] [--max N] [--model M]   # rewrites old session summaries via Haiku
  recall pin TEXT [--session SID|-] [--project DIR]     # always-on memory in inject
  recall unpin ID
  recall pins  [--session SID] [--project DIR|all]
  recall version

env:
  AGENT_MEMORY_DB   override default DB path`)
}

func cmdInject(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	var dbPath, project, query string
	var days, budget int
	var promptStdin bool
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "project directory (defaults to $PWD)")
	fs.IntVar(&days, "days", 30, "lookback days")
	fs.IntVar(&budget, "budget", 250, "approx token budget")
	fs.StringVar(&query, "query", "", "FTS5 query to surface relevant prior turns alongside the recall block")
	fs.BoolVar(&promptStdin, "prompt-stdin", false, "read user prompt from stdin JSON (UserPromptSubmit hook payload) and use it as --query")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if project == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		project = wd
	}
	var sessionID string
	if promptStdin {
		var payload struct {
			Prompt    string `json:"prompt"`
			CWD       string `json:"cwd"`
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(os.Stdin).Decode(&payload); err == nil {
			if payload.CWD != "" {
				project = payload.CWD
			}
			if query == "" {
				query = payload.Prompt
			}
			sessionID = payload.SessionID
		}
		// On decode error: silently degrade to a non-query inject. Hooks must
		// not block the user even if Claude Code changes the payload format.
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = inject.Render(ctx, st, inject.Options{
		Project:   project,
		SessionID: sessionID,
		Days:      days,
		Budget:    budget,
		Query:     query,
	}, os.Stdout)
	return err
}

// cmdPin records a pinned memory tied to a session (default) or project.
// Pins always surface at the top of inject for that session/project.
func cmdPin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	var dbPath, project, session string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "project_dir scope (default $PWD; '-' for global)")
	fs.StringVar(&session, "session", "", "session id (omit for project-scoped pin)")

	flagArgs, textArgs := splitFlagsAndText(args, []string{"db", "project", "session"})
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(textArgs, " "))
	if text == "" {
		return fmt.Errorf("usage: recall pin \"<text>\" [--session SID] [--project DIR]")
	}
	if len(text) > 500 {
		text = text[:500]
	}
	switch project {
	case "":
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		project = wd
	case "-":
		project = ""
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var id int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var ierr error
		id, ierr = tx.CreatePin(store.Pin{
			SessionID:  session,
			ProjectDir: project,
			Ts:         time.Now().Unix(),
			Text:       text,
		})
		return ierr
	}); err != nil {
		return err
	}
	scope := project
	if session != "" {
		scope = "session=" + session
	} else if scope == "" {
		scope = "(global)"
	}
	fmt.Printf("pinned #%d  scope=%s\n  %s\n", id, scope, text)
	return nil
}

func cmdUnpin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("unpin", flag.ExitOnError)
	var dbPath string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: recall unpin <id>")
	}
	var id int64
	if _, err := fmt.Sscanf(fs.Arg(0), "%d", &id); err != nil || id <= 0 {
		return fmt.Errorf("invalid pin id %q", fs.Arg(0))
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.ClearPin(id)
	}); err != nil {
		return err
	}
	fmt.Printf("unpinned #%d\n", id)
	return nil
}

func cmdPins(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pins", flag.ExitOnError)
	var dbPath, project, session string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "filter by project_dir (default $PWD; 'all' for everything)")
	fs.StringVar(&session, "session", "", "filter by session id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if project == "" {
		wd, _ := os.Getwd()
		project = wd
	} else if project == "all" {
		project = ""
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	pins, err := st.ActivePins(ctx, session, project, 100)
	if err != nil {
		return err
	}
	if len(pins) == 0 {
		fmt.Println("(no active pins)")
		return nil
	}
	for _, p := range pins {
		scope := p.ProjectDir
		if p.SessionID != "" {
			scope = "session=" + p.SessionID
		} else if scope == "" {
			scope = "(global)"
		}
		fmt.Printf("#%-4d  scope=%s\n        %s\n", p.ID, scope, p.Text)
	}
	return nil
}

// cmdSearch is FTS5 search across past turns. Mirrors the recall_search MCP
// tool so shell users (and agents that reach for Bash before MCP) get the
// same surface. Output is one hit per line by default; --json for structured.
// cmdSearch runs hybrid recall (FTS5 turns + cosine decisions, fused via
// Reciprocal Rank Fusion). Modes: 'hybrid' (default), 'lexical', 'semantic'.
// Mirrors the recall_search MCP tool one-to-one so shell users and agents
// see the same answers.
func cmdSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	var dbPath, project, mode, entity string
	var limit int
	var jsonOut, hyde, noHyDE bool
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "filter by project_dir (default $PWD with ancestor scope; 'all' for everything)")
	fs.StringVar(&mode, "mode", "hybrid", "hybrid | lexical | semantic")
	fs.IntVar(&limit, "limit", 10, "max results")
	fs.StringVar(&entity, "entity", "", "filter to items mentioning this @entity (case-insensitive; the leading @ is optional)")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of a table")
	fs.BoolVar(&hyde, "hyde", false, "force HyDE expansion on the semantic channel")
	fs.BoolVar(&noHyDE, "no-hyde", false, "force-disable HyDE")

	flagArgs, textArgs := splitFlagsAndText(args, []string{"db", "project", "mode", "limit", "entity"})
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(textArgs, " "))
	if query == "" {
		return fmt.Errorf("usage: recall search \"<query>\" [--mode hybrid|lexical|semantic] [--project DIR|all] [--limit N]")
	}
	switch project {
	case "":
		wd, _ := os.Getwd()
		project = wd
	case "all":
		project = ""
	}

	useHyDE := os.Getenv("ANTHROPIC_API_KEY") != ""
	if hyde {
		useHyDE = true
	}
	if noHyDE {
		useHyDE = false
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var emb recall.Embedder
	if mode != "lexical" {
		e, err := embed.New("", embed.Options{})
		if err == nil {
			emb = e
		}
	}

	res, err := recall.Search(ctx, st.DB(), recall.SearchOptions{
		Query:    query,
		Project:  project,
		Limit:    limit,
		Mode:     mode,
		Embedder: emb,
		UseHyDE:  useHyDE,
		Entity:   strings.TrimPrefix(entity, "@"),
	})
	if err != nil {
		return err
	}

	if jsonOut {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if res.Count == 0 {
		fmt.Println("(no matches)")
		return nil
	}
	if res.ExpandedQuery != "" {
		fmt.Printf("hyde: %s → %q\n", res.EmbedderName, res.ExpandedQuery)
	}
	for _, h := range res.Hits {
		day := h.Ts
		if len(day) > 10 {
			day = day[:10]
		}
		channels := strings.Join(h.Channels, "+")
		switch h.Kind {
		case "decision":
			fmt.Printf("%-12s [%-10s #%-3d %s]  %s\n", day, h.DecisionKind, h.DecisionID, channels, truncate(h.Text, 100))
		case "turn":
			fmt.Printf("%-12s [%-10s %s %s]  %s\n", day, h.Role, truncate(h.SessionID, 8), channels, h.Excerpt)
		}
	}
	return nil
}

// cmdRecordToolEvent handles the PostToolUse hook. Reads a JSON payload from
// stdin (Anthropic's documented hook envelope) and:
//
//   - For Bash with non-zero exit / is_error / interrupted: record a
//     "surprise" decision so the agent learns "this command failed here."
//   - For Edit/Write: record before/after content hashes in the tool_events
//     table. If the same file is edited again within 10 minutes with hashes
//     that swap (before<->after of a prior edit), that's a revert — record
//     a high-salience surprise decision capturing the mistake.
//
// Hooks must never break the session, so all errors are logged and we exit 0.
func cmdRecordToolEvent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("record-tool-event", flag.ExitOnError)
	var dbPath string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var payload struct {
		HookEvent    string          `json:"hook_event_name"`
		ToolName     string          `json:"tool_name"`
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
		CWD          string          `json:"cwd"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&payload); err != nil {
		fmt.Fprintf(os.Stderr, "record-tool-event: decode stdin: %v\n", err)
		return nil
	}

	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "record-tool-event: open db: %v\n", err)
		return nil
	}
	defer st.Close()

	switch payload.ToolName {
	case "Bash":
		handleBashEvent(ctx, st, payload.CWD, payload.ToolInput, payload.ToolResponse)
	case "Edit", "Write", "MultiEdit":
		handleEditEvent(ctx, st, payload.CWD, payload.ToolName, payload.ToolInput, payload.ToolResponse)
	}
	return nil
}

func handleBashEvent(ctx context.Context, st *store.Store, cwd string, in, resp json.RawMessage) {
	if !looksLikeBashFailure(resp) {
		return
	}
	cmdLine := bashCommand(in)
	if cmdLine == "" {
		return
	}
	excerpt := bashFailureExcerpt(resp)
	text := fmt.Sprintf("bash failed in this project: `%s`", truncate(cmdLine, 120))
	if excerpt != "" {
		text += " — " + truncate(excerpt, 160)
	}
	if len(text) > 500 {
		text = text[:500]
	}
	insertSurpriseDecision(ctx, st, cwd, text)
}

// handleEditEvent records the edit and looks for an inverse pair within
// the last 10 minutes (revert detection). If the current edit's
// before_hash matches a prior edit's after_hash AND the current edit's
// after_hash matches the prior edit's before_hash, the agent reverted
// itself — surface that as a learning event.
func handleEditEvent(ctx context.Context, st *store.Store, cwd, tool string, in, resp json.RawMessage) {
	if looksLikeFailure(resp) {
		// failed edit — record as a Bash-style failure, no revert tracking
		path := fileFieldFromInput(in)
		text := fmt.Sprintf("%s failed for `%s`", tool, truncate(path, 120))
		insertSurpriseDecision(ctx, st, cwd, text)
		return
	}
	path, before, after := editStrings(in)
	if path == "" || before == "" || after == "" {
		return
	}
	beforeHash := hashHex(before)
	afterHash := hashHex(after)
	now := time.Now().Unix()

	// Look for a recent inverse: a prior edit on this file where before
	// matches our after AND after matches our before — that's a revert.
	var priorID int64
	err := st.DB().QueryRowContext(ctx, `
		SELECT id FROM tool_events
		 WHERE file_path = ? AND tool IN ('Edit','Write','MultiEdit')
		   AND ts > ? AND before_hash = ? AND after_hash = ?
		 ORDER BY ts DESC LIMIT 1`,
		path, now-10*60, afterHash, beforeHash,
	).Scan(&priorID)
	if err == nil && priorID > 0 {
		text := fmt.Sprintf("edit was reverted within 10min: %s — agent's first edit may have been wrong",
			truncate(path, 200))
		insertSurpriseDecision(ctx, st, cwd, text)
	}

	_, _ = st.DB().ExecContext(ctx, `
		INSERT INTO tool_events(ts, cwd, tool, file_path, before_hash, after_hash)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?)`,
		now, cwd, tool, path, beforeHash, afterHash)
}

func insertSurpriseDecision(ctx context.Context, st *store.Store, cwd, text string) {
	_ = st.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO decisions(project_dir, ts, kind, text, source, salience)
			VALUES (NULLIF(?, ''), ?, 'feedback', ?, 'surprise', 2.0)`,
			cwd, time.Now().Unix(), text)
		return err
	})
}

func editStrings(in json.RawMessage) (path, before, after string) {
	var s struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"` // Write tool
	}
	_ = json.Unmarshal(in, &s)
	path = s.FilePath
	before = s.OldString
	after = s.NewString
	if after == "" && s.Content != "" {
		after = s.Content
	}
	return
}

func fileFieldFromInput(in json.RawMessage) string {
	var s struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(in, &s)
	return s.FilePath
}

// looksLikeFailure handles the same shapes as looksLikeBashFailure plus
// "permission denied" / "patch did not apply" patterns from Edit responses.
func looksLikeFailure(resp json.RawMessage) bool {
	if looksLikeBashFailure(resp) {
		return true
	}
	// Treat free-form error strings in the response as failure too.
	var s struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(resp, &s)
	if s.Error != "" || s.Type == "error" {
		return true
	}
	low := strings.ToLower(s.Message)
	for _, needle := range []string{"failed", "error", "denied", "did not apply"} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func looksLikeBashFailure(resp json.RawMessage) bool {
	if len(resp) == 0 {
		return false
	}
	// Try a few shapes — Anthropic has shifted these between versions.
	var s struct {
		Success     *bool  `json:"success"`
		IsError     *bool  `json:"is_error"`
		Error       string `json:"error"`
		ExitCode    *int   `json:"exit_code"`
		Interrupted *bool  `json:"interrupted"`
	}
	_ = json.Unmarshal(resp, &s)
	switch {
	case s.Success != nil && !*s.Success:
		return true
	case s.IsError != nil && *s.IsError:
		return true
	case s.ExitCode != nil && *s.ExitCode != 0:
		return true
	case s.Interrupted != nil && *s.Interrupted:
		return true
	case s.Error != "":
		return true
	}
	return false
}

func bashCommand(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var s struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &s)
	return strings.TrimSpace(s.Command)
}

func bashFailureExcerpt(resp json.RawMessage) string {
	var s struct {
		Stderr string `json:"stderr"`
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(resp, &s)
	for _, candidate := range []string{s.Stderr, s.Error, s.Output} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			return strings.Split(candidate, "\n")[0]
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func cmdIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	var source, root, dbPath string
	fs.StringVar(&source, "source", "all", "source kind: all | claude-code | codex | cursor")
	fs.StringVar(&root, "root", "", "source root path (defaults to source-specific; ignored for source=all)")
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Printf("ingest db=%s\n", dbPath)

	if source == "all" || source == "claude-code" {
		r := root
		if r == "" {
			r = claude.DefaultRoot()
		}
		t0 := time.Now()
		s, err := claude.Ingest(ctx, st, r)
		dur := time.Since(t0)
		fmt.Printf("[claude-code] root=%s\n", r)
		fmt.Printf("  files=%d sessions=%d lines: processed=%d skipped=%d\n",
			s.Files, s.Sessions, s.LinesProcessed, s.LinesSkipped)
		fmt.Printf("  new: turns=%d files=%d decisions=%d  errors=%d duration=%s\n",
			s.NewTurns, s.NewFiles, s.NewDecisions, s.Errors, dur)
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "codex" {
		p := root
		if p == "" || source == "all" {
			p = codex.DefaultPath()
		}
		s, err := codex.Ingest(ctx, st, p)
		if !s.DBExists {
			fmt.Printf("[codex]       db=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[codex]       db=%s\n", p)
			fmt.Printf("  threads=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Threads, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "cursor" {
		p := root
		if p == "" || source == "all" {
			p = cursor.DefaultPath()
		}
		s, err := cursor.Ingest(ctx, st, p)
		if !s.DBExists {
			fmt.Printf("[cursor]      db=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[cursor]      db=%s\n", p)
			fmt.Printf("  composers=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Composers, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "user-memory" {
		p := root
		if p == "" || source == "all" {
			p = usermemory.DefaultRoot()
		}
		s, err := usermemory.Ingest(ctx, st, p)
		fmt.Printf("[user-memory] root=%s\n", p)
		fmt.Printf("  dirs=%d files=%d new: decisions=%d  errors=%d duration=%s\n",
			s.MemoryDirs, s.Files, s.NewDecisions, s.Errors, s.Duration)
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "hermes" {
		p := root
		if p == "" || source == "all" {
			p = hermes.DefaultPath()
		}
		s, err := hermes.Ingest(ctx, st, p)
		if !s.DBExists {
			fmt.Printf("[hermes]      db=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[hermes]      db=%s\n", p)
			fmt.Printf("  sessions=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Sessions, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "cline" {
		p := root
		if p == "" || source == "all" {
			p = cline.DefaultRoot()
		}
		s, err := cline.Ingest(ctx, st, p)
		if !s.RootExists {
			fmt.Printf("[cline]       root=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[cline]       root=%s\n", p)
			fmt.Printf("  tasks=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Tasks, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "aider" {
		// --root may be a comma-separated list of paths for aider —
		// individual users keep code in 2-3 different layouts (e.g.
		// ~/code AND ~/work). When empty, fall back to the curated
		// DefaultRoots() set.
		var roots []string
		if root == "" || source == "all" {
			roots = aider.DefaultRoots()
		} else {
			for _, p := range strings.Split(root, ",") {
				if p = strings.TrimSpace(p); p != "" {
					roots = append(roots, p)
				}
			}
		}
		s, err := aider.IngestAll(ctx, st, roots)
		fmt.Printf("[aider]       roots=%v\n", roots)
		fmt.Printf("  files=%d sessions=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
			s.Files, s.Sessions, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "continue" {
		p := root
		if p == "" || source == "all" {
			p = continueide.DefaultRoot()
		}
		s, err := continueide.Ingest(ctx, st, p)
		if !s.RootExists {
			fmt.Printf("[continue]    root=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[continue]    root=%s\n", p)
			fmt.Printf("  sessions=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Sessions, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "goose" {
		p := root
		if p == "" || source == "all" {
			p = goose.DefaultPath()
		}
		s, err := goose.Ingest(ctx, st, p)
		if !s.DBExists {
			fmt.Printf("[goose]       db=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[goose]       db=%s\n", p)
			fmt.Printf("  sessions=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Sessions, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "zed" {
		p := root
		if p == "" || source == "all" {
			p = zed.DefaultPath()
		}
		s, err := zed.Ingest(ctx, st, p)
		if !s.DBExists {
			fmt.Printf("[zed]         db=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[zed]         db=%s\n", p)
			fmt.Printf("  threads=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Threads, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "roocode" {
		p := root
		if p == "" || source == "all" {
			p = roocode.DefaultRoot()
		}
		s, err := roocode.Ingest(ctx, st, p)
		if !s.RootExists {
			fmt.Printf("[roocode]     root=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[roocode]     root=%s\n", p)
			fmt.Printf("  tasks=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Tasks, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	if source == "all" || source == "kilocode" {
		p := root
		if p == "" || source == "all" {
			p = kilocode.DefaultRoot()
		}
		s, err := kilocode.Ingest(ctx, st, p)
		if !s.RootExists {
			fmt.Printf("[kilocode]    root=%s (not found — skipping)\n", p)
		} else {
			fmt.Printf("[kilocode]    root=%s\n", p)
			fmt.Printf("  tasks=%d new: sessions=%d turns=%d  errors=%d duration=%s\n",
				s.Tasks, s.NewSessions, s.NewTurns, s.Errors, s.Duration)
		}
		if err != nil {
			return err
		}
	}

	switch source {
	case "all", "claude-code", "codex", "cursor", "user-memory",
		"hermes", "cline", "aider", "continue", "goose", "zed",
		"roocode", "kilocode":
		// known source — handled above
	default:
		return fmt.Errorf("source %q not supported (try all | claude-code | codex | cursor | user-memory | hermes | cline | aider | continue | goose | zed | roocode | kilocode)", source)
	}
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	var interval time.Duration
	var dbPath string
	fs.DurationVar(&interval, "interval", 30*time.Second, "polling interval")
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "watch: shutting down")
		cancel()
	}()

	fmt.Printf("recall watch: interval=%s db=%s\n", interval, dbPath)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	tickOnce(ctx, st)
	maybeMaintain(ctx, st)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			tickOnce(ctx, st)
			maybeMaintain(ctx, st)
		}
	}
}

// maintainInterval gates the housekeeping pass so it runs at most once a day
// even though the watch loop ticks every ~30s.
const maintainInterval = 24 * time.Hour

// maybeMaintain runs the dedup + decay pass if it hasn't run in the last
// maintainInterval. Best-effort: a maintenance failure must never take down
// the ingest daemon, so errors are logged and swallowed.
func maybeMaintain(ctx context.Context, st *store.Store) {
	last, err := st.LastMaintainAt(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintain: read marker:", err)
		return
	}
	if !last.IsZero() && time.Since(last) < maintainInterval {
		return
	}
	stats, err := maintain.Run(ctx, st, maintain.Options{Dedup: true, Decay: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintain:", err)
		return // don't advance the marker, so we retry next tick
	}
	if err := st.SetMaintainAt(ctx, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "maintain: set marker:", err)
	}
	if stats.Merged > 0 || stats.Decayed > 0 {
		fmt.Printf("%s  maintain: merged=%d decayed=%d in %s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			stats.Merged, stats.Decayed, stats.Duration.Truncate(time.Millisecond))
	}

	// Optional daily entity-wiki refresh — only when the user opts in (it
	// spends Anthropic API budget). Gated by the same once-a-day marker via
	// being inside maybeMaintain's post-gate body.
	if os.Getenv("RECALL_AUTO_WIKI") != "" && os.Getenv("ANTHROPIC_API_KEY") != "" {
		emb, _ := embed.New("", embed.Options{})
		ws, werr := wiki.Run(ctx, st, emb, wiki.Options{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
		if werr != nil {
			fmt.Fprintln(os.Stderr, "wiki:", werr)
		} else if ws.Built > 0 {
			fmt.Printf("%s  wiki: built=%d skipped=%d in %s\n",
				time.Now().UTC().Format("2006-01-02T15:04:05Z"),
				ws.Built, ws.Skipped, ws.Duration.Truncate(time.Millisecond))
		}
	}
}

// cmdMaintain runs the dedup + decay housekeeping pass on demand. The watch
// daemon runs this automatically once a day; this is the manual entry point
// (and supports --dry-run to preview).
func cmdMaintain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("maintain", flag.ExitOnError)
	var dbPath, project string
	var dedupThreshold, decayFloor float64
	var decayDays int
	var noDedup, noDecay, dryRun, jsonOut bool
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "limit to a project_dir with ancestor scoping (default: all projects)")
	fs.Float64Var(&dedupThreshold, "dedup-threshold", maintain.DefaultDedupThreshold, "cosine at/above which two decisions are merged")
	fs.Float64Var(&decayFloor, "decay-floor", maintain.DefaultDecayFloor, "effective-salience below which stale auto-extracted rows are aged out")
	fs.IntVar(&decayDays, "decay-days", maintain.DefaultDecayDays, "minimum days unused before a row can be aged out")
	fs.BoolVar(&noDedup, "no-dedup", false, "skip the dedup pass")
	fs.BoolVar(&noDecay, "no-decay", false, "skip the decay pass")
	fs.BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of human text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if project == "-" {
		project = "" // explicit "all projects"
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := maintain.Run(ctx, st, maintain.Options{
		Project:        project,
		DedupThreshold: dedupThreshold,
		DecayFloor:     decayFloor,
		DecayDays:      decayDays,
		Dedup:          !noDedup,
		Decay:          !noDecay,
		DryRun:         dryRun,
	})
	if err != nil {
		return err
	}
	if !dryRun {
		_ = st.SetMaintainAt(ctx, time.Now())
	}

	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{
			"merged": stats.Merged, "decayed": stats.Decayed,
			"dry_run": dryRun, "duration_ms": stats.Duration.Milliseconds(),
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%smaintain: merged=%d  decayed=%d  (%s)\n",
		prefix, stats.Merged, stats.Decayed, stats.Duration.Truncate(time.Millisecond))
	return nil
}

// cmdWiki builds / shows / lists the auto-curated entity wiki. Building needs
// ANTHROPIC_API_KEY (Haiku distillation); --show and --list are local-only.
func cmdWiki(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("wiki", flag.ExitOnError)
	var dbPath, show string
	var list, dryRun, jsonOut bool
	var minMentions, max int
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&show, "show", "", "show the card for an entity by name (no build)")
	fs.BoolVar(&list, "list", false, "list entities that already have cards (no build)")
	fs.IntVar(&minMentions, "min-mentions", 3, "only build cards for entities with >= this many mentions")
	fs.IntVar(&max, "max", 50, "cap entities processed per run")
	fs.BoolVar(&dryRun, "dry-run", false, "build but don't store")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of human text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// --show: print one card.
	if show != "" {
		card, found, err := st.GetEntityCard(ctx, show)
		if err != nil {
			return err
		}
		if !found {
			fmt.Printf("(no card for @%s — run `recall wiki` to build, or it has < %d mentions)\n", strings.TrimPrefix(show, "@"), minMentions)
			return nil
		}
		if jsonOut {
			b, _ := json.MarshalIndent(card, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		fmt.Printf("@%s  (%d sources, refreshed %s)\n  %s\n",
			card.Display, card.SourceCount,
			time.Unix(card.RefreshedAt, 0).Format("2006-01-02"), card.Summary)
		return nil
	}

	// --list: enumerate existing cards.
	if list {
		cards, err := st.ListEntityCards(ctx, max)
		if err != nil {
			return err
		}
		if jsonOut {
			b, _ := json.MarshalIndent(cards, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		if len(cards) == 0 {
			fmt.Println("(no entity cards yet — run `recall wiki` to build)")
			return nil
		}
		for _, c := range cards {
			fmt.Printf("@%-20s %s\n", c.Display, truncate(c.Summary, 100))
		}
		return nil
	}

	// Default: build cards.
	emb, _ := embed.New("", embed.Options{}) // optional — cards still build without embeddings
	stats, err := wiki.Run(ctx, st, emb, wiki.Options{
		APIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		MinMentions: minMentions,
		Max:         max,
		DryRun:      dryRun,
	})
	if err != nil {
		return err
	}
	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{
			"considered": stats.Considered, "built": stats.Built,
			"skipped": stats.Skipped, "errors": stats.Errors,
			"dry_run": dryRun, "duration_ms": stats.Duration.Milliseconds(),
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%swiki: considered=%d built=%d skipped=%d errors=%d  (%s)\n",
		prefix, stats.Considered, stats.Built, stats.Skipped, stats.Errors,
		stats.Duration.Truncate(time.Millisecond))
	return nil
}

func tickOnce(ctx context.Context, st *store.Store) {
	t0 := time.Now()
	cstats, cerr := claude.Ingest(ctx, st, claude.DefaultRoot())
	xstats, xerr := codex.Ingest(ctx, st, codex.DefaultPath())
	rstats, rerr := cursor.Ingest(ctx, st, cursor.DefaultPath())
	ustats, uerr := usermemory.Ingest(ctx, st, usermemory.DefaultRoot())
	hstats, herr := hermes.Ingest(ctx, st, hermes.DefaultPath())
	nstats, nerr := cline.Ingest(ctx, st, cline.DefaultRoot())
	astats, aerr := aider.IngestAll(ctx, st, aider.DefaultRoots())
	istats, ierr := continueide.Ingest(ctx, st, continueide.DefaultRoot())
	gstats, gerr := goose.Ingest(ctx, st, goose.DefaultPath())
	zstats, zerr := zed.Ingest(ctx, st, zed.DefaultPath())
	rkstats, rkerr := roocode.Ingest(ctx, st, roocode.DefaultRoot())
	kkstats, kkerr := kilocode.Ingest(ctx, st, kilocode.DefaultRoot())
	// Compact one-line log per tick. Errors ride along on stderr individually.
	fmt.Printf("%s  claude:new(t=%d f=%d d=%d) codex:new(s=%d t=%d) cursor:new(s=%d t=%d) user-mem:new(d=%d) hermes:new(s=%d t=%d) cline:new(s=%d t=%d) aider:new(s=%d t=%d) continue:new(s=%d t=%d) goose:new(s=%d t=%d) zed:new(s=%d t=%d) roo:new(s=%d t=%d) kilo:new(s=%d t=%d) total=%s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		cstats.NewTurns, cstats.NewFiles, cstats.NewDecisions,
		xstats.NewSessions, xstats.NewTurns,
		rstats.NewSessions, rstats.NewTurns,
		ustats.NewDecisions,
		hstats.NewSessions, hstats.NewTurns,
		nstats.NewSessions, nstats.NewTurns,
		astats.NewSessions, astats.NewTurns,
		istats.NewSessions, istats.NewTurns,
		gstats.NewSessions, gstats.NewTurns,
		zstats.NewSessions, zstats.NewTurns,
		rkstats.NewSessions, rkstats.NewTurns,
		kkstats.NewSessions, kkstats.NewTurns,
		time.Since(t0).Truncate(time.Microsecond))
	for _, e := range []error{cerr, xerr, rerr, uerr, herr, nerr, aerr, ierr, gerr, zerr, rkerr, kkerr} {
		if e != nil {
			fmt.Fprintln(os.Stderr, "tick err:", e)
		}
	}
}

// cmdDecide records a durable decision/fact/preference from the shell side.
// Defaults to a fact at salience 1.5 (above pattern-extracted decisions at 0.5),
// scoped to $PWD. Pass --project=- to scope globally instead.
//
// Accepts flags interleaved with positional text — `recall decide "..." --kind fact`
// works as well as `recall decide --kind fact "..."`. Flag.Parse alone stops at
// the first positional, which is hostile when the obvious phrasing puts the
// quoted text first; we pre-split.
func cmdDecide(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("decide", flag.ExitOnError)
	var dbPath, kind, project string
	var salience float64
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&kind, "kind", "fact", "fact | preference | feedback")
	fs.StringVar(&project, "project", "", "project_dir scope (default $PWD, '-' for global)")
	fs.Float64Var(&salience, "salience", 1.5, "salience score (1.0 = neutral, >1 boost, <1 deprioritise)")

	flagArgs, textArgs := splitFlagsAndText(args, []string{"db", "kind", "project", "salience"})
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(textArgs, " "))
	if text == "" {
		return fmt.Errorf("usage: recall decide \"<text>\" [--kind ...] [--project ...]")
	}
	if len(text) > 500 {
		text = text[:500]
	}
	switch project {
	case "":
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		project = wd
	case "-":
		project = "" // global
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Reconsolidation: embed and check for similar existing decisions.
	// Best-effort — if the embedder isn't reachable, fall back to a
	// plain insert so users without an embedder still get the CLI.
	emb, _ := embed.New("", embed.Options{})
	var rec *reconsolidate.Result
	if emb != nil {
		rec, _ = reconsolidate.Run(ctx, st, emb, project, text)
	}

	// Trust feedback loop: a near-duplicate (cosine ≥ reinforceThreshold) is a
	// re-assertion of an existing fact — reinforce its confidence instead of
	// inserting a near-identical row.
	if rec != nil {
		if best, ok := rec.BestReinforced(); ok {
			if conf, ferr := st.RecordFeedback(ctx, best.ID, "confirmed", "re-asserted via recall decide"); ferr == nil {
				fmt.Printf("reinforced existing decision #%d  (cos=%.3f, confidence now %.2f)\n  %s\n",
					best.ID, best.Score, conf, truncate(best.Text, 100))
				return nil
			}
		}
	}

	var id int64
	now := time.Now().Unix()
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var ierr error
		id, ierr = tx.InsertDecision(store.Decision{
			ProjectDir: project,
			Ts:         now,
			Kind:       kind,
			Text:       text,
			Source:     "cli",
			Salience:   salience,
		})
		if ierr != nil {
			return ierr
		}
		if err := entities.IndexInTx(tx, entities.KindDecision, fmt.Sprintf("%d", id), text, now); err != nil {
			return err
		}
		if rec != nil && len(rec.Embedding) > 0 {
			if err := tx.SetEmbedding(id, rec.Embedding); err != nil {
				return err
			}
			oldIDs := make([]int64, 0, len(rec.Superseded))
			for _, m := range rec.Superseded {
				oldIDs = append(oldIDs, m.ID)
			}
			if err := tx.SupersedeDecisions(id, oldIDs); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	scope := project
	if scope == "" {
		scope = "(global)"
	}
	fmt.Printf("recorded decision #%d  [%s salience=%.1f]  scope=%s\n  %s\n",
		id, kind, salience, scope, text)
	if rec != nil {
		if len(rec.Superseded) > 0 {
			fmt.Println("  superseded (auto, similarity ≥0.85):")
			for _, m := range rec.Superseded {
				// Lower the superseded fact's confidence — it was contradicted.
				_, _ = st.RecordFeedback(ctx, m.ID, "contradicted", "superseded via recall decide")
				fmt.Printf("    #%d  cos=%.3f  %s\n", m.ID, m.Score, truncate(m.Text, 80))
			}
		}
		if len(rec.Candidates) > 0 {
			fmt.Println("  related (not actioned, 0.65–0.85):")
			for _, m := range rec.Candidates {
				fmt.Printf("    #%d  cos=%.3f  %s\n", m.ID, m.Score, truncate(m.Text, 80))
			}
		}
	}
	return nil
}

// splitFlagsAndText separates args into (flagArgs, positionalArgs). Flags are
// recognised by name (with or without value glued via '='); positionals are
// returned in order. Unrecognised --foo gets passed to flag.Parse so it errors
// loudly rather than being silently treated as text.
func splitFlagsAndText(args []string, names []string) (flagArgs, text []string) {
	known := map[string]bool{}
	for _, n := range names {
		known["--"+n] = true
		known["-"+n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if eq := strings.Index(a, "="); eq > 0 {
				flagArgs = append(flagArgs, a)
				continue
			}
			if known[a] && i+1 < len(args) {
				flagArgs = append(flagArgs, a, args[i+1])
				i++
				continue
			}
			// unknown flag — let flag.Parse complain
			flagArgs = append(flagArgs, a)
			continue
		}
		text = append(text, a)
	}
	return
}

func cmdDecisions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("decisions", flag.ExitOnError)
	var dbPath, project, kind string
	var limit int
	var jsonOut bool
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&project, "project", "", "project_dir filter (default $PWD with ancestor scoping; 'all' for everything)")
	fs.StringVar(&kind, "kind", "", "filter: fact | preference | feedback")
	fs.IntVar(&limit, "limit", 50, "max results")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var (
		where   = "superseded_by IS NULL"
		sqlArgs []any
	)
	switch project {
	case "all":
		// no filter
	case "":
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		where += " AND (project_dir = ? OR project_dir IS NULL OR ? LIKE (project_dir || '/%'))"
		sqlArgs = append(sqlArgs, wd, wd)
	default:
		where += " AND project_dir = ?"
		sqlArgs = append(sqlArgs, project)
	}
	if kind != "" {
		where += " AND kind = ?"
		sqlArgs = append(sqlArgs, kind)
	}
	sqlArgs = append(sqlArgs, limit)

	q := fmt.Sprintf(`
		SELECT id, kind, source, COALESCE(project_dir, ''), salience,
		       COALESCE(confidence, 0.5), COALESCE(ts, 0), text
		  FROM decisions
		 WHERE %s
		 ORDER BY salience DESC, COALESCE(ts, 0) DESC
		 LIMIT ?`, where)

	rows, err := st.DB().QueryContext(ctx, q, sqlArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		ID         int64   `json:"id"`
		Kind       string  `json:"kind"`
		Source     string  `json:"source"`
		ProjectDir string  `json:"project_dir,omitempty"`
		Salience   float64 `json:"salience"`
		Confidence float64 `json:"confidence"`
		TS         int64   `json:"ts,omitempty"`
		Text       string  `json:"text"`
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Kind, &r.Source, &r.ProjectDir, &r.Salience, &r.Confidence, &r.TS, &r.Text); err != nil {
			return err
		}
		out = append(out, r)
	}
	if jsonOut {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(out) == 0 {
		fmt.Println("(no decisions)")
		return nil
	}
	for _, r := range out {
		scope := r.ProjectDir
		if scope == "" {
			scope = "(global)"
		}
		fmt.Printf("#%-4d  [%-10s sal=%.1f  conf=%.2f  src=%-7s]  %s\n        scope: %s\n",
			r.ID, r.Kind, r.Salience, r.Confidence, r.Source, r.Text, scope)
	}
	return nil
}

func cmdForget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forget", flag.ExitOnError)
	var dbPath string
	var decisionID int64
	var patternText string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.Int64Var(&decisionID, "decision", 0, "delete a single decision by id")
	fs.StringVar(&patternText, "pattern-text", "", "delete all pattern-source decisions whose text matches exactly")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if decisionID == 0 && patternText == "" {
		return fmt.Errorf("usage: recall forget --decision <id> | --pattern-text \"<text>\"")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var n int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var res sql.Result
		var err error
		if decisionID != 0 {
			res, err = tx.Exec(`DELETE FROM decisions WHERE id = ?`, decisionID)
		} else {
			res, err = tx.Exec(`DELETE FROM decisions WHERE LOWER(text) = LOWER(?) AND source = 'pattern'`, patternText)
		}
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("deleted %d decision(s)\n", n)
	return nil
}

// cmdInstallModel side-loads pre-staged ONNX assets from a local directory
// (no internet required). Use this when a security team mirrors the
// artifacts internally and hands users a directory of approved files.
// Each file is SHA256-verified against the pinned hash; mismatches abort.
func cmdInstallModel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install-model", flag.ExitOnError)
	var src string
	fs.StringVar(&src, "from", "", "directory containing the side-loaded artifacts (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if src == "" {
		return fmt.Errorf("usage: recall install-model --from <dir>")
	}
	if err := embed.InstallFromDir(src, os.Stdout); err != nil {
		return err
	}
	fmt.Println("done. all four artifacts verified + installed.")
	return nil
}

// cmdParaphrase walks active decisions and generates K alternate phrasings
// per decision via Haiku, embedding each and storing in
// `decision_paraphrases`. The semantic-search channel queries against
// canonical + paraphrase embeddings and takes the max cosine per
// decision, so paraphrases broaden recall without polluting precision.
//
// Idempotent: skips decisions that already have at least minPerDecision
// paraphrases. Requires ANTHROPIC_API_KEY (graceful no-op without it).
func cmdParaphrase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paraphrase", flag.ExitOnError)
	var dbPath string
	var limit, perDecision int
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.IntVar(&limit, "limit", 50, "max decisions to process per run")
	fs.IntVar(&perDecision, "per-decision", 4, "how many paraphrases per decision")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set — needed to call Haiku for paraphrase generation")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	emb, err := embed.New("", embed.Options{})
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}

	// Decisions that don't yet have at least perDecision paraphrases.
	rows, err := st.DB().QueryContext(ctx, `
		SELECT d.id, d.text
		  FROM decisions d
		 WHERE d.superseded_by IS NULL
		   AND (
		     SELECT COUNT(*) FROM decision_paraphrases p
		      WHERE p.decision_id = d.id AND p.embedding IS NOT NULL
		   ) < ?
		 ORDER BY d.salience DESC
		 LIMIT ?`, perDecision, limit)
	if err != nil {
		return err
	}
	type job struct {
		id   int64
		text string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.text); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()

	t0 := time.Now()
	totalGen := 0
	totalEmb := 0
	for _, j := range jobs {
		paraphrases, err := recall.GenerateParaphrases(ctx, j.text, perDecision)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: paraphrase id=%d: %v\n", j.id, err)
			continue
		}
		if len(paraphrases) == 0 {
			continue
		}
		for _, p := range paraphrases {
			v, err := emb.Embed(ctx, p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: embed paraphrase: %v\n", err)
				continue
			}
			blob := vec.Encode(v)
			if err := st.Tx(ctx, func(tx *store.Tx) error {
				_, err := tx.InsertParaphrase(store.Paraphrase{
					DecisionID: j.id,
					Text:       p,
					Embedding:  blob,
				})
				return err
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warn: store paraphrase: %v\n", err)
				continue
			}
			totalEmb++
		}
		totalGen += len(paraphrases)
	}
	fmt.Printf("paraphrase: decisions=%d paraphrases=%d embedded=%d duration=%s\n",
		len(jobs), totalGen, totalEmb, time.Since(t0).Truncate(time.Millisecond))
	return nil
}

// cmdDownloadModel fetches the ONNX Runtime dylib + the all-MiniLM-L6-v2
// model + tokenizer files into ~/.local/share/recall/. Idempotent:
// already-present files are skipped unless --force.
func cmdDownloadModel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("download-model", flag.ExitOnError)
	var force bool
	fs.BoolVar(&force, "force", false, "redownload even if files exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t0 := time.Now()
	if err := embed.DownloadOnnxAssets(ctx, force, os.Stdout); err != nil {
		return err
	}
	fmt.Printf("done in %s\n", time.Since(t0).Truncate(time.Millisecond))
	fmt.Printf("model dir: %s\n", embed.ModelDir())
	fmt.Printf("runtime:   %s\n", embed.RuntimeLibPath())
	return nil
}

// cmdEmbed populates the embedding column for rows missing it. Provider
// defaults to Apple's NLEmbedding on darwin (on-device, no network, no
// asset download — friendly for security-controlled orgs). Pass
// --provider ollama for the cross-platform fallback. Idempotent: only
// processes rows where embedding IS NULL; re-runs pick up new rows.
func cmdEmbed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	var dbPath, provider, scope, ollama, model string
	var batch int
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&provider, "provider", "", "apple | ollama (default: apple on darwin, ollama elsewhere)")
	fs.StringVar(&scope, "scope", "decisions", "decisions | turns")
	fs.StringVar(&ollama, "ollama", "http://localhost:11434", "Ollama base URL (provider=ollama)")
	fs.StringVar(&model, "model", "nomic-embed-text", "Ollama model (provider=ollama)")
	fs.IntVar(&batch, "batch", 100, "max rows to embed per run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if scope != "decisions" && scope != "turns" {
		return fmt.Errorf("--scope must be 'decisions' or 'turns'")
	}

	emb, err := embed.New(provider, embed.Options{OllamaURL: ollama, OllamaModel: model})
	if err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var (
		query  string
		setSQL string
	)
	switch scope {
	case "decisions":
		query = `SELECT id, text FROM decisions WHERE embedding IS NULL ORDER BY salience DESC LIMIT ?`
		setSQL = `UPDATE decisions SET embedding = ? WHERE id = ?`
	case "turns":
		query = `SELECT rowid, text FROM turns WHERE embedding IS NULL AND text != '' AND length(text) BETWEEN 16 AND 8000 LIMIT ?`
		setSQL = `UPDATE turns SET embedding = ? WHERE rowid = ?`
	}

	rows, err := st.DB().QueryContext(ctx, query, batch)
	if err != nil {
		return err
	}
	defer rows.Close()

	type job struct {
		id   int64
		text string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.text); err != nil {
			return err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	t0 := time.Now()
	embedded := 0
	for _, j := range jobs {
		v, err := emb.Embed(ctx, j.text)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: embed id=%d: %v\n", j.id, err)
			continue
		}
		if _, err := st.DB().ExecContext(ctx, setSQL, vec.Encode(v), j.id); err != nil {
			fmt.Fprintf(os.Stderr, "warn: store id=%d: %v\n", j.id, err)
			continue
		}
		embedded++
	}
	fmt.Printf("embedded %d/%d %s rows via %s in %s\n",
		embedded, len(jobs), scope, emb.Name(), time.Since(t0).Truncate(time.Millisecond))
	return nil
}

// cmdConsolidate calls Anthropic Haiku to rewrite session summaries for
// sessions older than --days. Idempotent: only sessions whose
// summary_consolidated_at is older than ended_at get reprocessed.
// Requires ANTHROPIC_API_KEY (same key Claude Code uses).
func cmdConsolidate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("consolidate", flag.ExitOnError)
	var dbPath, model string
	var days, max int
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.StringVar(&model, "model", "", "Anthropic model (default claude-haiku-4-5-20251001)")
	fs.IntVar(&days, "days", 7, "consolidate sessions older than this many days")
	fs.IntVar(&max, "max", 25, "max sessions to process per run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set — needed to call Haiku for summarisation")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := consolidate.Run(ctx, st, consolidate.Options{
		APIKey:        apiKey,
		Model:         model,
		OlderThanDays: days,
		Max:           max,
	})
	fmt.Printf("consolidate: considered=%d updated=%d skipped=%d errors=%d duration=%s\n",
		stats.Considered, stats.Updated, stats.Skipped, stats.Errors, stats.Duration)
	return err
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	var dbPath string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Println("recall doctor:")
	r := doctor.Run(ctx, dbPath)
	r.Print(os.Stdout)
	if r.HasFailures() {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var dbPath string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	// ServeStdio blocks until the client disconnects (EOF on stdin).
	return mcpserver.Serve(st, version)
}

func cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var dbPath string
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("recall stats (db=%s)\n", dbPath)
	fmt.Printf("  sessions: %d\n", s.Sessions)
	fmt.Printf("  turns:    %d\n", s.Turns)
	fmt.Printf("  files:    %d\n", s.Files)
	fmt.Printf("  projects: %d\n", s.Projects)
	if !s.OldestSession.IsZero() {
		fmt.Printf("  range:    %s → %s\n",
			s.OldestSession.Format("2006-01-02"),
			s.NewestSession.Format("2006-01-02"))
	}
	return nil
}

// cmdBench runs offline retrieval benchmarks. Currently only LongMemEval is
// supported. Uses a working SQLite at $TMPDIR/recall-bench.sqlite (truncated
// per question) so the user's main DB is never touched.
func cmdBench(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: recall bench longmemeval <data.json> [--limit N] [--lex-only] [--db PATH] [--json]")
		return fmt.Errorf("bench: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "longmemeval":
		return cmdBenchLongMemEval(ctx, rest)
	default:
		return fmt.Errorf("bench: unknown subcommand %q (try: longmemeval)", sub)
	}
}

func cmdBenchLongMemEval(ctx context.Context, args []string) error {
	var (
		dbPath     string
		limit      int
		skip       int
		lexOnly    bool
		jsonOut    bool
		tune       bool
		devSize    int
		rrfk       int
		weights    string
		rerank     bool
		rerankTopK int
	)
	fs := flag.NewFlagSet("bench longmemeval", flag.ExitOnError)
	fs.StringVar(&dbPath, "db", filepath.Join(os.TempDir(), "recall-bench-longmemeval.sqlite"),
		"working SQLite path (will be truncated per question)")
	fs.IntVar(&limit, "limit", 0, "max questions to evaluate (0 = all)")
	fs.IntVar(&skip, "skip", 0, "skip first N questions before evaluating (for held-out splits)")
	fs.BoolVar(&lexOnly, "lex-only", false, "skip semantic channel (no embedder)")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of human text")
	fs.BoolVar(&tune, "tune", false, "split into dev (--dev-size questions) + holdout, sweep RRF k and per-channel weights on dev, evaluate winner on holdout")
	fs.IntVar(&devSize, "dev-size", 50, "first N questions used as the tuning dev split when --tune is set")
	fs.IntVar(&rrfk, "rrf-k", 0, "RRF damping constant (0 = recall.DefaultK = 60)")
	fs.StringVar(&weights, "weights", "", "comma-separated channel weights for [lex,sem,temp,keyword] (e.g. '1,1,0.5,1.5'); empty = all 1.0")
	fs.BoolVar(&rerank, "rerank", false, "after RRF fusion, run a Haiku rerank pass over the top --rerank-topk hits (requires ANTHROPIC_API_KEY)")
	fs.IntVar(&rerankTopK, "rerank-topk", 5, "number of top fused hits sent to the rerank model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("bench longmemeval: need exactly one positional arg (data.json)")
	}
	dataPath := fs.Arg(0)
	parsedWeights, err := parseWeights(weights)
	if err != nil {
		return err
	}

	// Throwaway working DB; nuke any leftover from a previous run so the
	// bench starts clean even after a SIGINT mid-run.
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	var emb recall.Embedder
	if !lexOnly {
		e, err := embed.New("", embed.Options{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: no embedder available (%v) — falling back to lex+temporal only\n", err)
		} else {
			emb = e
		}
	}

	if tune {
		return cmdBenchTune(ctx, bench.Options{
			DataPath: dataPath,
			DBPath:   dbPath,
			Embedder: emb,
			Progress: os.Stderr,
		}, devSize, rrfk, parsedWeights, jsonOut)
	}

	res, err := bench.Run(ctx, bench.Options{
		DataPath:   dataPath,
		DBPath:     dbPath,
		Embedder:   emb,
		Limit:      limit,
		Skip:       skip,
		Verbose:    jsonOut,
		Progress:   os.Stderr,
		RRFk:       rrfk,
		Weights:    parsedWeights,
		Rerank:     rerank,
		RerankTopK: rerankTopK,
	})
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Print(bench.FormatHuman(res))
	return nil
}

// parseWeights parses "1,1.5,0.5,2" into []float64{1,1.5,0.5,2}. Empty
// string returns nil (= caller-default = all 1.0). Each entry must
// parse cleanly as a non-negative float; any error fails the command
// before the bench starts so users don't burn 20 minutes on a typo.
func parseWeights(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("--weights[%d] = %q: %v", i, p, err)
		}
		if v < 0 {
			return nil, fmt.Errorf("--weights[%d] = %v: must be non-negative", i, v)
		}
		out = append(out, v)
	}
	return out, nil
}

// cmdBenchTune does coordinate-descent on the dev split, then evaluates
// the winning config on the held-out remainder.
//
// Plain greedy: start at k=60, weights=[1,1,1,1]; sweep one axis at a
// time, pick the value that maximizes dev R@5, fix it, move to the next
// axis. One pass through k + 4 weights = 5 axes × 5 values ≈ 25 evals
// at 50q each. ~30s/eval (per-q includes turn embedding) → ~12 minutes.
// Final holdout eval is one more 450-q run, ~17 minutes.
func cmdBenchTune(ctx context.Context, base bench.Options, devSize, initialRRFk int, initialWeights []float64, jsonOut bool) error {
	if devSize <= 0 {
		return fmt.Errorf("--dev-size must be > 0")
	}

	weights := []float64{1, 1, 1, 1}
	if len(initialWeights) > 0 {
		copy(weights, initialWeights)
	}
	rrfk := initialRRFk
	if rrfk <= 0 {
		rrfk = recall.DefaultK
	}

	type evalResult struct {
		R5  float64
		R1  float64
		R10 float64
	}
	runDev := func(k int, w []float64) (evalResult, error) {
		opts := base
		opts.Limit = devSize
		opts.Skip = 0
		opts.RRFk = k
		opts.Weights = append([]float64(nil), w...)
		opts.Progress = nil
		r, err := bench.Run(ctx, opts)
		if err != nil {
			return evalResult{}, err
		}
		return evalResult{R5: r.RecallAt(5), R1: r.RecallAt(1), R10: r.RecallAt(10)}, nil
	}

	// Sweep grids — keep small so a tuning run takes minutes, not hours.
	// Wider sweeps can be done with --weights '...' on individual eval runs.
	kCandidates := []int{30, 60, 120}
	weightCandidates := []float64{0.5, 1.0, 2.0}
	channelNames := []string{"lex", "sem", "temp", "kw"}

	fmt.Fprintln(os.Stderr, "tuning on dev split (first ", devSize, " questions)")
	fmt.Fprintf(os.Stderr, "starting config: k=%d weights=%v\n", rrfk, weights)

	baseEval, err := runDev(rrfk, weights)
	if err != nil {
		return fmt.Errorf("baseline dev eval: %w", err)
	}
	bestR5 := baseEval.R5
	fmt.Fprintf(os.Stderr, "  baseline dev R@5 = %.1f%%\n", 100*bestR5)

	// Tune k first (cheaper to find the right k, then weights).
	bestK := rrfk
	for _, k := range kCandidates {
		if k == bestK {
			continue
		}
		ev, err := runDev(k, weights)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  k=%-3d  dev R@5 = %.1f%%\n", k, 100*ev.R5)
		if ev.R5 > bestR5 {
			bestR5 = ev.R5
			bestK = k
		}
	}
	rrfk = bestK
	fmt.Fprintf(os.Stderr, "→ best k = %d  (dev R@5 = %.1f%%)\n", rrfk, 100*bestR5)

	// Coordinate-descent over weights, one channel at a time.
	for chIdx, name := range channelNames {
		bestW := weights[chIdx]
		for _, w := range weightCandidates {
			if w == bestW {
				continue
			}
			trial := append([]float64(nil), weights...)
			trial[chIdx] = w
			ev, err := runDev(rrfk, trial)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  %s=%.1f  dev R@5 = %.1f%%\n", name, w, 100*ev.R5)
			if ev.R5 > bestR5 {
				bestR5 = ev.R5
				bestW = w
			}
		}
		weights[chIdx] = bestW
		fmt.Fprintf(os.Stderr, "→ best %s weight = %.1f  (dev R@5 = %.1f%%)\n", name, bestW, 100*bestR5)
	}

	fmt.Fprintf(os.Stderr, "\nfinal dev config: k=%d weights=%v  dev R@5=%.1f%%\n",
		rrfk, weights, 100*bestR5)
	fmt.Fprintln(os.Stderr, "evaluating on holdout (questions", devSize, "..end)")

	holdout := base
	holdout.Skip = devSize
	holdout.Limit = 0 // all remaining
	holdout.RRFk = rrfk
	holdout.Weights = weights
	holdout.Progress = os.Stderr
	res, err := bench.Run(ctx, holdout)
	if err != nil {
		return fmt.Errorf("holdout eval: %w", err)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"dev_size":     devSize,
			"dev_r5":       bestR5,
			"best_k":       rrfk,
			"best_weights": weights,
			"holdout":      res,
		})
	}
	fmt.Println("=== TUNING SUMMARY ===")
	fmt.Printf("dev split:    first %d questions, R@5 = %.1f%%\n", devSize, 100*bestR5)
	fmt.Printf("best k:       %d\n", rrfk)
	fmt.Printf("best weights: lex=%.1f sem=%.1f temp=%.1f kw=%.1f\n", weights[0], weights[1], weights[2], weights[3])
	fmt.Println()
	fmt.Println("=== HOLDOUT ===")
	fmt.Print(bench.FormatHuman(res))
	return nil
}

// cmdDistill scans recent turns for durable infrastructure facts and runbook
// procedures via Haiku and writes them back as decisions. Idempotent —
// distilled turns are flagged via turns.distilled_at, and decisions
// dedupe on (text, project_dir, source-bucket) via InsertDecisionIfNew.
func cmdDistill(ctx context.Context, args []string) error {
	var (
		dbPath    string
		days      int
		batchSize int
		maxTurns  int
		dryRun    bool
		jsonOut   bool
	)
	fs := flag.NewFlagSet("distill", flag.ExitOnError)
	fs.StringVar(&dbPath, "db", defaultDBPath(), "database path")
	fs.IntVar(&days, "days", 0, "lookback window (days). 0 = all undistilled turns")
	fs.IntVar(&batchSize, "batch", 20, "turns per Haiku call")
	fs.IntVar(&maxTurns, "max-turns", 500, "safety cap on turns processed per run")
	fs.BoolVar(&dryRun, "dry-run", false, "log what would be written, but don't insert decisions")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of human text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := distill.Run(ctx, st, distill.Options{
		Days:      days,
		BatchSize: batchSize,
		MaxTurns:  maxTurns,
		DryRun:    dryRun,
		Verbose:   os.Stderr,
	})
	if err != nil {
		return err
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "warn: ANTHROPIC_API_KEY unset — distill is a no-op without an API key")
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Printf("distill complete in %s\n", res.Duration.Truncate(time.Millisecond))
	fmt.Printf("  turns read:    %d\n", res.TurnsRead)
	fmt.Printf("  turns scanned: %d\n", res.TurnsScanned)
	fmt.Printf("  batches sent:  %d\n", res.BatchesSent)
	fmt.Printf("  items found:   %d\n", res.Items)
	if dryRun {
		fmt.Printf("  inserted:     0  (dry-run; would have inserted %d)\n", res.Items)
	} else {
		fmt.Printf("  inserted:     %d  (deduped against existing decisions)\n", res.Inserted)
	}
	if res.Errors > 0 {
		fmt.Printf("  errors:       %d  (see stderr)\n", res.Errors)
	}
	return nil
}
