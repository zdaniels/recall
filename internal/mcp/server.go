// Package mcp implements a minimal MCP stdio server for recall.
//
// We deliberately do not pull in a third-party MCP library. The protocol is
// line-delimited JSON-RPC 2.0 over stdin/stdout, and we only need four methods:
// initialize, notifications/initialized, tools/list, tools/call. Keeping this
// in stdlib eliminates supply-chain exposure on the agent-facing wire format.
//
// Spec reference: modelcontextprotocol.io (Tools, Lifecycle).
package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/recall/internal/embed"
	"github.com/zdaniels/recall/internal/entities"
	"github.com/zdaniels/recall/internal/inject"
	"github.com/zdaniels/recall/internal/recall"
	"github.com/zdaniels/recall/internal/reconsolidate"
	"github.com/zdaniels/recall/internal/store"
	"github.com/zdaniels/recall/internal/vec"
)

const protocolVersion = "2025-03-26"

// Tool is a single MCP-exposed function.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args map[string]any) (*Result, error)
}

// Result is what a tool handler produces. Either Text OR JSON should be set;
// JSON is encoded as a text content block (per MCP convention).
type Result struct {
	Text    string
	JSON    any
	IsError bool
}

// Serve runs an MCP stdio session against st until stdin EOF.
func Serve(st *store.Store, version string) error {
	srv := newServer("recall", version, tools(st))
	return srv.run(context.Background())
}

// --- internals ---

type server struct {
	name, version string
	tools         map[string]Tool
	in            io.Reader
	out           io.Writer
	log           io.Writer
	outMu         sync.Mutex
}

func newServer(name, version string, ts []Tool) *server {
	s := &server{
		name:    name,
		version: version,
		tools:   make(map[string]Tool, len(ts)),
		in:      os.Stdin,
		out:     os.Stdout,
		log:     os.Stderr,
	}
	for _, t := range ts {
		s.tools[t.Name] = t
	}
	return s
}

type rpcMsg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcErr          `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	errParse       = -32700
	errInvalid     = -32600
	errMethod      = -32601
	errInvalidArgs = -32602
	errInternal    = -32603
)

func (s *server) run(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 1<<16), 16<<20) // up to 16MB messages
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			s.writeErr(nil, errParse, "parse error", err.Error())
			continue
		}
		s.dispatch(ctx, msg)
	}
	return sc.Err()
}

func (s *server) dispatch(ctx context.Context, msg rpcMsg) {
	switch msg.Method {
	case "initialize":
		s.writeResult(msg.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		})
	case "notifications/initialized", "initialized":
		// notification — no response
	case "ping":
		s.writeResult(msg.ID, map[string]any{})
	case "tools/list":
		names := make([]string, 0, len(s.tools))
		for n := range s.tools {
			names = append(names, n)
		}
		sort.Strings(names)
		list := make([]map[string]any, 0, len(names))
		for _, n := range names {
			t := s.tools[n]
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		s.writeResult(msg.ID, map[string]any{"tools": list})
	case "tools/call":
		s.handleToolsCall(ctx, msg)
	default:
		if msg.ID != nil {
			s.writeErr(msg.ID, errMethod, "method not found", msg.Method)
		}
	}
}

func (s *server) handleToolsCall(ctx context.Context, msg rpcMsg) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.writeErr(msg.ID, errInvalidArgs, "invalid params", err.Error())
		return
	}
	t, ok := s.tools[p.Name]
	if !ok {
		s.writeErr(msg.ID, errMethod, "unknown tool", p.Name)
		return
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}

	// Tool errors surface as IsError text content, not as JSON-RPC errors —
	// that way the agent sees them and can adapt rather than the session breaking.
	res, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		s.writeResult(msg.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
		return
	}
	text := res.Text
	if res.JSON != nil {
		b, jerr := json.Marshal(res.JSON)
		if jerr != nil {
			s.writeErr(msg.ID, errInternal, "encode result", jerr.Error())
			return
		}
		text = string(b)
	}
	s.writeResult(msg.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": res.IsError,
	})
}

func (s *server) writeResult(id *json.RawMessage, result any) {
	s.send(rpcMsg{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) writeErr(id *json.RawMessage, code int, message, data string) {
	s.send(rpcMsg{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcErr{Code: code, Message: message, Data: data},
	})
}

func (s *server) send(m rpcMsg) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	enc := json.NewEncoder(s.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		fmt.Fprintf(s.log, "mcp encode: %v\n", err)
	}
}

// --- arg helpers ---

func argString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(s) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		// JSON-RPC clients sometimes send numbers as strings
		var x int
		fmt.Sscanf(n, "%d", &x)
		if x == 0 {
			return def
		}
		return x
	}
	return def
}

func requireString(args map[string]any, key string) (string, error) {
	v := strings.TrimSpace(argString(args, key, ""))
	if v == "" {
		return "", fmt.Errorf("missing required argument: %s", key)
	}
	return v, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// --- tool definitions ---

func tools(st *store.Store) []Tool {
	return []Tool{
		{
			Name:        "recall_search",
			Description: "Hybrid search across past session turns and active decisions. Default mode 'hybrid' fuses FTS5 lexical results (turns) and cosine semantic results (decisions, HyDE-expanded query) via Reciprocal Rank Fusion — strictly better than either alone. Set mode='lexical' for FTS-only or mode='semantic' for cosine-only. Prefer this over grep'ing files; use this whenever you need to recall what was discussed, decided, or learned.",
			InputSchema: schemaObject(
				schemaProp("query", "string", "Natural-language query. FTS5 operators (phrases, AND/OR, NOT, prefix*) work in lexical/hybrid modes."),
				schemaProp("mode", "string", "'hybrid' (default) | 'lexical' | 'semantic'."),
				schemaProp("project", "string", "Filter to one project_dir (absolute path)."),
				schemaProp("limit", "integer", "Max results (default 10, max 50)."),
				schemaProp("hyde", "boolean", "Use HyDE query expansion in the semantic channel. Default true when ANTHROPIC_API_KEY is set; harmless to leave on."),
				schemaProp("entity", "string", "Restrict to items mentioning this @entity (case-insensitive; leading @ optional). Returns no hits if the entity has never been mentioned."),
			).required("query"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) { return handleSearch(ctx, st, args) },
		},
		{
			Name:        "recall_files",
			Description: "Files recently touched in a project, ordered by recency × frequency. Use to discover the relevant working set without grepping the tree.",
			InputSchema: schemaObject(
				schemaProp("project", "string", "Absolute project_dir."),
				schemaProp("days", "integer", "Lookback window in days (default 30)."),
				schemaProp("limit", "integer", "Max results (default 20, max 100)."),
			).required("project"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) { return handleFiles(ctx, st, args) },
		},
		{
			Name:        "recall_sessions",
			Description: "Recent sessions with summaries, optionally project-scoped. Use to recover context from prior conversations.",
			InputSchema: schemaObject(
				schemaProp("project", "string", "Absolute project_dir filter."),
				schemaProp("days", "integer", "Lookback days (default 30)."),
				schemaProp("limit", "integer", "Max results (default 10, max 50)."),
			),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) { return handleSessions(ctx, st, args) },
		},
		{
			Name:        "recall_summary",
			Description: "Compact recall block for a project — same content the SessionStart hook injects. Returns markdown.",
			InputSchema: schemaObject(
				schemaProp("project", "string", "Absolute project_dir."),
				schemaProp("days", "integer", "Lookback days (default 30)."),
				schemaProp("budget", "integer", "Approx token budget (default 250)."),
			).required("project"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) { return handleSummary(ctx, st, args) },
		},
		{
			Name:        "recall_semantic_search",
			Description: "Find decisions semantically similar to a query (cosine similarity on Ollama-generated embeddings). Use when text-match would miss synonyms ('DB' vs 'database', 'auth' vs 'authentication'). Requires `recall embed` to have been run; falls back to an explanatory error if no embeddings exist or Ollama is unreachable.",
			InputSchema: schemaObject(
				schemaProp("query", "string", "Natural language query."),
				schemaProp("project", "string", "Filter to project_dir (with ancestor scoping)."),
				schemaProp("limit", "integer", "Max results (default 10, max 50)."),
			).required("query"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) {
				return handleSemanticSearch(ctx, st, args)
			},
		},
		{
			Name:        "recall_decisions",
			Description: "List active decisions / preferences / facts for a project (including ancestor scope and globals). Use when you need to remember the user's stated preferences before acting.",
			InputSchema: schemaObject(
				schemaProp("project", "string", "Absolute project_dir."),
				schemaProp("kind", "string", "Filter: 'feedback' | 'preference' | 'fact'."),
				schemaProp("limit", "integer", "Max results (default 20, max 100)."),
			).required("project"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) {
				return handleRecallDecisions(ctx, st, args)
			},
		},
		{
			Name:        "pin_for_session",
			Description: "Pin a short note to the top of every inject for the rest of this session (or the project if no session_id provided). Use when the user emphasises something they want kept top-of-mind across context resets — e.g. 'remember the API base is ...', 'goal for today: ...', 'don't forget to roll back the migration before lunch'. Returns the pin id; the user can clear with `recall unpin <id>`.",
			InputSchema: schemaObject(
				schemaProp("text", "string", "≤ 500 chars. Be concise — pins are always-on context."),
				schemaProp("session_id", "string", "Session to scope to. If omitted, pin is project-scoped."),
				schemaProp("project", "string", "Project dir for project-scoped pins."),
			).required("text"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) {
				return handlePinForSession(ctx, st, args)
			},
		},
		{
			Name:        "record_decision",
			Description: "Persist a durable decision, fact, or preference into recall. Use sparingly — only when the user has clearly committed to a choice or stated a preference. If the text is a near-duplicate of an existing fact it reinforces that fact's confidence (returns {reinforced, confidence}) instead of adding a row; if it contradicts one, the old fact is superseded and its confidence lowered. Otherwise returns the new decision id.",
			InputSchema: schemaObject(
				schemaProp("text", "string", "The decision in human-readable form, ≤ 500 chars."),
				schemaProp("kind", "string", "'preference' | 'fact' | 'feedback' (default 'fact')."),
				schemaProp("project", "string", "project_dir scope; omit for a global decision."),
			).required("text"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) {
				return handleRecordDecision(ctx, st, args)
			},
		},
		{
			Name:        "recall_wiki",
			Description: "Fetch the auto-curated reference card for an @-entity (a person, service, repo, tool, or concept) — a rolled-up summary of everything durably known about it across all sessions. Use for 'what is X / who is X / tell me about X' when X is a recurring named thing. Returns not-found if the entity has no card yet (too few mentions, or `recall wiki` hasn't run).",
			InputSchema: schemaObject(
				schemaProp("entity", "string", "Entity name, with or without a leading '@'."),
			).required("entity"),
			Handler: func(ctx context.Context, args map[string]any) (*Result, error) {
				return handleRecallWiki(ctx, st, args)
			},
		},
	}
}

// schema helpers — these build minimal JSON Schema descriptions inline.

type schema map[string]any

func schemaObject(props ...schema) schema {
	merged := map[string]any{}
	for _, p := range props {
		for k, v := range p {
			merged[k] = v
		}
	}
	return schema{
		"type":       "object",
		"properties": merged,
	}
}

func schemaProp(name, typ, desc string) schema {
	return schema{name: map[string]any{"type": typ, "description": desc}}
}

func (s schema) required(fields ...string) schema {
	s["required"] = fields
	return s
}

// --- tool handlers ---

type searchHit struct {
	SessionID  string `json:"session_id"`
	ProjectDir string `json:"project_dir,omitempty"`
	Role       string `json:"role"`
	Ts         string `json:"ts,omitempty"`
	Excerpt    string `json:"excerpt"`
}

// handleSearch implements hybrid retrieval: lexical FTS over turns +
// semantic cosine over decisions, fused via RRF. Mode override picks one
// or the other. Uses HyDE on the semantic channel by default when
// ANTHROPIC_API_KEY is set.
func handleSearch(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	project := argString(args, "project", "")
	limit := clampInt(argInt(args, "limit", 10), 1, 50)
	mode := argString(args, "mode", recall.ModeHybrid)
	switch mode {
	case recall.ModeHybrid, recall.ModeLexical, recall.ModeSemantic:
	default:
		return &Result{Text: fmt.Sprintf("invalid mode %q (use hybrid|lexical|semantic)", mode), IsError: true}, nil
	}
	useHyDE := argBool(args, "hyde", os.Getenv("ANTHROPIC_API_KEY") != "")

	// Resolve embedder once per call. Failures degrade to lexical-only.
	var emb recall.Embedder
	if mode != recall.ModeLexical {
		e, err := embed.New(envOr("EMBED_PROVIDER", ""), embed.Options{
			OllamaURL:   envOr("OLLAMA_URL", "http://localhost:11434"),
			OllamaModel: envOr("OLLAMA_MODEL", "nomic-embed-text"),
		})
		if err == nil {
			emb = e
		}
	}

	entity := strings.TrimPrefix(strings.TrimSpace(argString(args, "entity", "")), "@")
	res, err := recall.Search(ctx, st.DB(), recall.SearchOptions{
		Query:    query,
		Project:  project,
		Limit:    limit,
		Mode:     mode,
		Embedder: emb,
		UseHyDE:  useHyDE,
		Entity:   entity,
	})
	if err != nil {
		return nil, err
	}
	return &Result{JSON: res}, nil
}

type fileHit struct {
	Path     string `json:"path"`
	Count    int    `json:"count"`
	LastSeen string `json:"last_seen,omitempty"`
	LastOp   string `json:"last_op,omitempty"`
}

func handleFiles(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	project, err := requireString(args, "project")
	if err != nil {
		return nil, err
	}
	days := clampInt(argInt(args, "days", 30), 1, 365)
	limit := clampInt(argInt(args, "limit", 20), 1, 100)
	cutoff := time.Now().AddDate(0, 0, -days).Unix()

	rows, err := st.DB().QueryContext(ctx, `
		SELECT path, COUNT(*) AS n, MAX(COALESCE(ts, 0)) AS last_ts,
		       (SELECT op FROM files f2
		         WHERE f2.path = files.path AND f2.project_dir = files.project_dir
		         ORDER BY f2.ts DESC LIMIT 1) AS last_op
		  FROM files
		 WHERE project_dir = ? AND COALESCE(ts, 0) >= ?
		 GROUP BY path
		 ORDER BY last_ts DESC, n DESC
		 LIMIT ?`,
		project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []fileHit{}
	for rows.Next() {
		var h fileHit
		var lastTs int64
		var lastOp sql.NullString
		if err := rows.Scan(&h.Path, &h.Count, &lastTs, &lastOp); err != nil {
			return nil, err
		}
		if lastTs > 0 {
			h.LastSeen = time.Unix(lastTs, 0).UTC().Format(time.RFC3339)
		}
		h.LastOp = lastOp.String
		out = append(out, h)
	}
	return &Result{JSON: map[string]any{"project": project, "files": out, "count": len(out)}}, nil
}

type sessionHit struct {
	ID         string `json:"id"`
	ProjectDir string `json:"project_dir,omitempty"`
	Summary    string `json:"summary,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	TurnCount  int    `json:"turn_count"`
}

func handleSessions(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	project := argString(args, "project", "")
	days := clampInt(argInt(args, "days", 30), 1, 365)
	limit := clampInt(argInt(args, "limit", 10), 1, 50)
	cutoff := time.Now().AddDate(0, 0, -days).Unix()

	sqlArgs := []any{cutoff}
	where := "COALESCE(ended_at, started_at, 0) >= ?"
	if project != "" {
		where += " AND project_dir = ?"
		sqlArgs = append(sqlArgs, project)
	}
	sqlArgs = append(sqlArgs, limit)

	q := fmt.Sprintf(`
		SELECT s.id, COALESCE(s.project_dir, ''), COALESCE(s.summary, ''),
		       COALESCE(s.started_at, 0), COALESCE(s.ended_at, 0),
		       (SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id)
		  FROM sessions s
		 WHERE %s
		 ORDER BY ended_at DESC LIMIT ?`, where)

	rows, err := st.DB().QueryContext(ctx, q, sqlArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []sessionHit{}
	for rows.Next() {
		var h sessionHit
		var startedAt, endedAt int64
		if err := rows.Scan(&h.ID, &h.ProjectDir, &h.Summary, &startedAt, &endedAt, &h.TurnCount); err != nil {
			return nil, err
		}
		if startedAt > 0 {
			h.StartedAt = time.Unix(startedAt, 0).UTC().Format(time.RFC3339)
		}
		if endedAt > 0 {
			h.EndedAt = time.Unix(endedAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, h)
	}
	return &Result{JSON: map[string]any{"sessions": out, "count": len(out)}}, nil
}

func handleSummary(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	project, err := requireString(args, "project")
	if err != nil {
		return nil, err
	}
	days := clampInt(argInt(args, "days", 30), 1, 365)
	budget := clampInt(argInt(args, "budget", 250), 50, 4000)

	var sb strings.Builder
	wrote, err := inject.Render(ctx, st, inject.Options{
		Project: project,
		Days:    days,
		Budget:  budget,
	}, &sb)
	if err != nil {
		return nil, err
	}
	if !wrote {
		return &Result{Text: fmt.Sprintf("(no recall for %s)", project)}, nil
	}
	return &Result{Text: sb.String()}, nil
}

type decisionHit struct {
	ID                int64   `json:"id"`
	Kind              string  `json:"kind"`
	Text              string  `json:"text"`
	Source            string  `json:"source"`
	ProjectDir        string  `json:"project_dir,omitempty"`
	Salience          float64 `json:"salience"`
	Confidence        float64 `json:"confidence"`
	EffectiveSalience float64 `json:"effective_salience"`
	Ts                string  `json:"ts,omitempty"`
}

func handleRecallDecisions(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	project, err := requireString(args, "project")
	if err != nil {
		return nil, err
	}
	kind := argString(args, "kind", "")
	limit := clampInt(argInt(args, "limit", 20), 1, 100)

	sqlArgs := []any{project, project}
	where := `superseded_by IS NULL AND (project_dir = ? OR project_dir IS NULL OR ? LIKE (project_dir || '/%'))`
	if kind != "" {
		where += " AND kind = ?"
		sqlArgs = append(sqlArgs, kind)
	}
	sqlArgs = append(sqlArgs, limit)

	q := fmt.Sprintf(`
		SELECT id, kind, text, source, COALESCE(project_dir, ''), salience,
		       COALESCE(confidence, 0.5), COALESCE(ts, 0),
		       %s AS effective_salience
		  FROM decisions
		 WHERE %s
		 ORDER BY
		   CASE source WHEN 'pattern' THEN 1 ELSE 0 END,
		   %s DESC,
		   COALESCE(ts, 0) DESC
		 LIMIT ?`, store.EffectiveSalienceExpr, where, store.EffectiveSalienceExpr)

	rows, err := st.DB().QueryContext(ctx, q, sqlArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []decisionHit{}
	ids := []int64{}
	for rows.Next() {
		var h decisionHit
		var ts int64
		if err := rows.Scan(&h.ID, &h.Kind, &h.Text, &h.Source, &h.ProjectDir,
			&h.Salience, &h.Confidence, &ts, &h.EffectiveSalience); err != nil {
			return nil, err
		}
		if ts > 0 {
			h.Ts = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, h)
		ids = append(ids, h.ID)
	}
	st.BumpUseCount(ctx, ids)
	return &Result{JSON: map[string]any{"decisions": out, "count": len(out)}}, nil
}

// handleSemanticSearch embeds the query via the configured embedder
// (default: Apple NLEmbedding on darwin — on-device, no network) and
// cosine-ranks against decisions that already have embeddings.
//
// Provider can be overridden via the EMBED_PROVIDER env var
// ('apple' | 'ollama'). With ollama, OLLAMA_URL and OLLAMA_MODEL apply.
// Returns IsError=true with guidance when no embeddings exist or the
// provider is unreachable.
func handleSemanticSearch(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	project := argString(args, "project", "")
	limit := clampInt(argInt(args, "limit", 10), 1, 50)

	emb, err := embed.New(envOr("EMBED_PROVIDER", ""), embed.Options{
		OllamaURL:   envOr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel: envOr("OLLAMA_MODEL", "nomic-embed-text"),
	})
	if err != nil {
		return &Result{Text: "semantic search unavailable: " + err.Error(), IsError: true}, nil
	}
	queryVec, err := emb.Embed(ctx, query)
	if err != nil {
		return &Result{
			Text:    fmt.Sprintf("semantic search unavailable: %v", err),
			IsError: true,
		}, nil
	}

	where := "embedding IS NOT NULL AND superseded_by IS NULL"
	sqlArgs := []any{}
	if project != "" {
		where += " AND (project_dir = ? OR project_dir IS NULL OR ? LIKE (project_dir || '/%'))"
		sqlArgs = append(sqlArgs, project, project)
	}
	rows, err := st.DB().QueryContext(ctx,
		fmt.Sprintf(`SELECT id, kind, source, COALESCE(project_dir,''), text, embedding FROM decisions WHERE %s`, where),
		sqlArgs...)
	if err != nil {
		return nil, fmt.Errorf("scan decisions: %w", err)
	}
	defer rows.Close()

	type stored struct {
		id      int64
		kind    string
		source  string
		project string
		text    string
		emb     []float32
	}
	var pool []stored
	for rows.Next() {
		var s stored
		var blob []byte
		if err := rows.Scan(&s.id, &s.kind, &s.source, &s.project, &s.text, &blob); err != nil {
			return nil, err
		}
		s.emb = vec.Decode(blob)
		if len(s.emb) == 0 {
			continue
		}
		pool = append(pool, s)
	}
	if len(pool) == 0 {
		return &Result{
			Text:    "no embeddings present — run `recall embed --scope decisions` first",
			IsError: true,
		}, nil
	}

	cands := make(map[int64][]float32, len(pool))
	byID := make(map[int64]stored, len(pool))
	for _, s := range pool {
		cands[s.id] = s.emb
		byID[s.id] = s
	}
	hits := vec.TopK(queryVec, cands, limit)

	type out struct {
		ID      int64   `json:"id"`
		Kind    string  `json:"kind"`
		Source  string  `json:"source"`
		Project string  `json:"project_dir,omitempty"`
		Text    string  `json:"text"`
		Score   float32 `json:"score"`
	}
	results := make([]out, 0, len(hits))
	ids := make([]int64, 0, len(hits))
	for _, h := range hits {
		s := byID[h.ID]
		results = append(results, out{
			ID: s.id, Kind: s.kind, Source: s.source,
			Project: s.project, Text: s.text, Score: h.Score,
		})
		ids = append(ids, s.id)
	}
	st.BumpUseCount(ctx, ids)
	return &Result{JSON: map[string]any{"hits": results, "count": len(results), "provider": emb.Name()}}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handlePinForSession(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	text, err := requireString(args, "text")
	if err != nil {
		return nil, err
	}
	if len(text) > 500 {
		text = text[:500]
	}
	sessionID := argString(args, "session_id", "")
	project := argString(args, "project", "")

	var id int64
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var ierr error
		id, ierr = tx.CreatePin(store.Pin{
			SessionID:  sessionID,
			ProjectDir: project,
			Ts:         time.Now().Unix(),
			Text:       text,
		})
		return ierr
	}); err != nil {
		return nil, err
	}
	return &Result{JSON: map[string]any{
		"id": id, "text": text, "session_id": sessionID, "project": project,
	}}, nil
}

func handleRecordDecision(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	text, err := requireString(args, "text")
	if err != nil {
		return nil, err
	}
	if len(text) > 500 {
		text = text[:500]
	}
	kind := argString(args, "kind", "fact")
	project := argString(args, "project", "")

	// Reconsolidation: embed new text, find similar existing decisions.
	// Auto-supersede very-similar ones (≥0.85), surface medium matches
	// (0.65–0.85) as candidates so the agent can flag them to the user.
	emb, embErr := embed.New(envOr("EMBED_PROVIDER", ""), embed.Options{
		OllamaURL:   envOr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel: envOr("OLLAMA_MODEL", "nomic-embed-text"),
	})
	var rec *reconsolidate.Result
	if embErr == nil {
		rec, _ = reconsolidate.Run(ctx, st, emb, project, text)
	}

	// Trust feedback loop: if this is a near-duplicate of an existing fact
	// (cosine ≥ reinforceThreshold), reinforce that fact's confidence instead
	// of inserting a near-identical row. Keeps the store from growing
	// monotonically and makes corroborated facts rank higher over time.
	if rec != nil {
		if best, ok := rec.BestReinforced(); ok {
			conf, ferr := st.RecordFeedback(ctx, best.ID, "confirmed", "re-asserted via record_decision")
			if ferr == nil {
				return &Result{JSON: map[string]any{
					"reinforced": best.ID,
					"confidence": conf,
					"text":       best.Text,
					"score":      best.Score,
					"note":       "near-duplicate of an existing fact; reinforced it instead of adding a new row",
				}}, nil
			}
			// If reinforcement failed for any reason, fall through to a
			// normal insert rather than dropping the decision.
		}
	}

	var newID int64
	now := time.Now().Unix()
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var ierr error
		newID, ierr = tx.InsertDecision(store.Decision{
			ProjectDir: project,
			Ts:         now,
			Kind:       kind,
			Text:       text,
			Source:     "tool",
			Salience:   1.5,
		})
		if ierr != nil {
			return ierr
		}
		if err := entities.IndexInTx(tx, entities.KindDecision, fmt.Sprintf("%d", newID), text, now); err != nil {
			return err
		}
		if rec != nil && len(rec.Embedding) > 0 {
			if err := tx.SetEmbedding(newID, rec.Embedding); err != nil {
				return err
			}
			ids := make([]int64, 0, len(rec.Superseded))
			for _, m := range rec.Superseded {
				ids = append(ids, m.ID)
			}
			if err := tx.SupersedeDecisions(newID, ids); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	out := map[string]any{
		"id": newID, "text": text, "kind": kind, "project": project,
	}
	if rec != nil {
		if len(rec.Superseded) > 0 {
			out["superseded"] = matchesJSON(rec.Superseded)
			// A superseded fact was contradicted by this new one — lower its
			// confidence (and log it) so it ranks below the current belief
			// even while it stays queryable in the supersede chain.
			for _, m := range rec.Superseded {
				_, _ = st.RecordFeedback(ctx, m.ID, "contradicted", "superseded via record_decision")
			}
		}
		if len(rec.Candidates) > 0 {
			out["similar"] = matchesJSON(rec.Candidates)
		}
	}
	return &Result{JSON: out}, nil
}

func handleRecallWiki(ctx context.Context, st *store.Store, args map[string]any) (*Result, error) {
	name, err := requireString(args, "entity")
	if err != nil {
		return nil, err
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	card, found, err := st.GetEntityCard(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return &Result{JSON: map[string]any{
			"entity": name, "found": false,
			"note": "no wiki card for this entity yet (too few mentions, or `recall wiki` hasn't been run)",
		}}, nil
	}
	return &Result{JSON: map[string]any{
		"entity":       card.Display,
		"found":        true,
		"summary":      card.Summary,
		"source_count": card.SourceCount,
		"refreshed_at": time.Unix(card.RefreshedAt, 0).UTC().Format(time.RFC3339),
	}}, nil
}

func matchesJSON(ms []reconsolidate.Match) []map[string]any {
	out := make([]map[string]any, len(ms))
	for i, m := range ms {
		out[i] = map[string]any{"id": m.ID, "text": m.Text, "score": m.Score}
	}
	return out
}

// silence unused
var _ = errors.New
