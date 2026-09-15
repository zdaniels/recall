# recall — design

## Mission

A single local-first memory substrate that the user's coding agents — Claude Code, Codex, Cursor, Hermes, and seven more — read from and write to. It should *feel* like the agent already knows you — your projects, your decisions, your preferences — without ever being told "remember this."

The bar is not "search index over transcripts." The bar is **real memory**: structured, salience-weighted, consolidated over time, recalled associatively, and surfaced seamlessly.

## Design principles

- **Automatic, not manual.** Memory is captured from session storage and tool events as they happen — never "remember this" friction.
- **Structured, not flat.** Decisions, files, sessions, edges, pins, embeddings — each kind has its own schema and ranking semantics.
- **Local-first.** SQLite on disk, no cloud sync, no daemon required (a watch daemon is opt-in).
- **Cross-tool.** Read-only adapters into eleven coding agents' session stores normalize into one DB. Writes only ever go to our own DB.
- **Cheap to surface.** Inject a compact, query-aware block at session start; pull more on demand via MCP tools mid-session.
- **Reinforcement-aware.** Salience decays with time, boosts with use, and survives reconsolidation when contradicted.

## Implementation status

What's landed:

| Surface | Status | Where |
|---|---|---|
| SQLite store (WAL, FTS5, schema migrations to v9) | ✅ | `internal/store` |
| Claude Code JSONL adapter (turns, files, decisions, edges) | ✅ | `internal/adapter/claude` |
| Codex SQLite adapter (`threads` → sessions + first_user_message) | ✅ | `internal/adapter/codex` |
| Cursor state.vscdb adapter (`composer.composerHeaders` + `cursorDiskKV.bubbleId:*` for per-turn content) | ✅ | `internal/adapter/cursor` |
| MEMORY.md sync (curated user memory → decisions, salience 2.5) | ✅ | `internal/adapter/usermemory` |
| Pattern-detected decisions (5 regexes) | ✅ | `internal/decisions` |
| `<private>` tag stripping (FTS + decisions both) | ✅ | `internal/textutil` |
| Project-aware inject block w/ ancestor scoping + decision IDs | ✅ | `internal/inject` |
| Salience math: `base × exp(-age/30d) + 0.1·ln(1+use_count)` | ✅ | `internal/store` (SQL expression) |
| Use-count tracking on read (inject + MCP recall_decisions) | ✅ | `internal/store.BumpUseCount` |
| Edit-then-revert detection (10-min window, hash-based) | ✅ | `cmd/recall/main.go`, `tool_events` table |
| Bash-failure surprise capture (PostToolUse hook) | ✅ | same |
| Edges co-occurrence graph populated at ingest | ✅ — file↔session, file↔file pairs | `internal/store.UpsertEdge` |
| Vector storage + pure-Go cosine similarity | ✅ — `embedding BLOB` columns | `internal/vec` |
| Embedder provider abstraction (apple, onnx) | ✅ | `internal/embed` |
| ONNX embedder (sentence-transformers/all-MiniLM-L6-v2 q8 arm64) — **default after `recall download-model`** | ✅ | `internal/embed/onnx.go` |
| Apple NLEmbedding fallback (built into macOS, zero-config baseline) | ✅ | `internal/embed/apple_darwin.go` |
| Hand-rolled BERT WordPiece tokenizer (~210 LOC) | ✅ — verified vs HF reference | `internal/embed/tokenizer.go` |
| Our own ONNX Runtime CGO bridge (no third-party Go bindings) | ✅ — vendored MIT header + ~250 LOC C wrapper + ~250 LOC Go | `internal/onnx/` |
| `recall download-model` (SHA256-pinned fetch from huggingface.co + github.com/microsoft/onnxruntime) | ✅ | `internal/embed/assets.go` |
| `recall install-model --from <dir>` (airgap / mirror install with hash verification) | ✅ | same |
| `recall_semantic_search` MCP tool | ✅ | `internal/mcp` |
| Reconsolidation: auto-supersede contradicting decisions (cosine ≥0.85) | ✅ — surfaces 0.65–0.85 matches as "related" | `internal/reconsolidate` |
| Trust/confidence scoring on decisions (schema v8) | ✅ — `confidence ∈ [0,1]` folds into `EffectiveSalienceExpr` as a 0.6–1.4× multiplier (neutral at the 0.5 default). Rises on corroboration, falls on contradiction/staleness; every change logged to `decision_feedback` | `internal/store`, `internal/reconsolidate` |
| Reinforcement on re-assertion (cosine ≥0.92) | ✅ — `record_decision` / `recall decide` reinforce an existing fact's confidence instead of inserting a near-duplicate; contradictions (0.85–0.92) supersede and lower the old fact's confidence | `internal/mcp`, `cmd/recall`, `internal/reconsolidate` |
| Ground-truth directive in inject + `recall_summary` | ✅ — explicit "treat these as established; prefer over re-deriving/asking" preamble above pins/instructions/decisions; corroborated facts marked `✓` | `internal/inject` |
| Scheduled maintenance pass (dedup + decay) | ✅ — `recall maintain` merges semantic near-duplicates (cosine ≥0.92, per-project) and ages out stale low-salience auto-extracted rows; folded into `recall watch` as a once-daily gate. Operates on stored embeddings (no network) | `internal/maintain`, `cmd/recall` |
| Auto-curated entity wiki (schema v9) | ✅ — `recall wiki` Haiku-distills one rolled-up card per @entity from its mentions, embeds it, refreshes incrementally as new mentions land; surfaced via the `recall_wiki` MCP tool + `recall wiki --show`. Opt-in (`ANTHROPIC_API_KEY`); optional daily auto-refresh in `recall watch` behind `RECALL_AUTO_WIKI` | `internal/wiki`, `internal/mcp`, `cmd/recall` |
| Spreading-activation in inject (file ↔ file co-occurrence neighbors) | ✅ — "Often touched alongside" section | `internal/inject` |
| `recall consolidate` — Haiku rewrite of old session summaries | ✅ — opt-in, requires `ANTHROPIC_API_KEY` | `internal/consolidate` |
| Hybrid `recall_search` (FTS5 + cosine fused via Reciprocal Rank Fusion) | ✅ — `mode=hybrid\|lexical\|semantic`, default hybrid | `internal/recall/search.go`, `internal/recall/rrf.go` |
| HyDE query expansion on the semantic channel | ✅ — Haiku generates a hypothetical answer, we embed that. In-process LRU cache, 1h TTL. Auto-on when `ANTHROPIC_API_KEY` set; graceful fallback otherwise | `internal/recall/hyde.go` |
| `instruction` memory type for procedural runbook content | ✅ — renders in its own section above Decisions | `internal/inject` |
| Async paraphrase generation per decision | ✅ — `recall paraphrase` (or auto-trigger from `record_decision`) generates 3-5 alternate phrasings via Haiku and embeds each. Semantic search takes max cosine across canonical + paraphrase embeddings per decision | `internal/recall/paraphrase.go`, schema v5 `decision_paraphrases` |
| Load-time SHA256 integrity check on ONNX assets | ✅ — dylib + model + tokenizer re-verified at `newONNX()`, not just at download | `internal/embed/onnx.go` |
| Pinning (`recall pin`, `pin_for_session` MCP tool) | ✅ — schema v4 `pins` table | `internal/store` |
| Session-aware inject ("This session so far" section) | ✅ — needs `session_id` from hook stdin JSON | `internal/inject` |
| Default inject budget 1500 tokens at SessionStart only | ✅ | `~/.claude/settings.json` |
| Pull-based mid-session recall (SessionStart inject only; agent uses MCP tools afterward) | ✅ | `~/.claude/settings.json`, `~/.claude/CLAUDE.md` |
| MCP stdio server (stdlib JSON-RPC, no third-party MCP dep) | ✅ — 9 tools | `internal/mcp` |
| `recall doctor` self-check (binary, DB, ONNX assets, per-tool wiring) | ✅ | `internal/doctor` |
| `recall watch` poll daemon + launchd plist template | ✅ | `cmd/recall`, `dist/` |
| Claude Code wiring (3 hooks + MCP server registration via `~/.claude.json`) | ✅ | `~/.claude/settings.json`, `~/.claude.json` |
| Tests | ✅ — 50+ subtests across 9 packages | adjacent `*_test.go` |
| `SBOM.md` for security-review consumption | ✅ | `SBOM.md` |
| Temporal-recency RRF channel (3rd channel in hybrid mode) | ✅ — re-ranks the union of FTS + cosine candidates by recency | `internal/recall/search.go` |
| Entity scoping (`@<name>` mention extraction + `--entity` filter) | ✅ — schema v6 `entities` + `entity_mentions`, populated on every adapter ingest path and decision insertion | `internal/entities`, `internal/recall/search.go` |
| Haiku rerank pass (`--rerank` on `recall search` and the bench) | ✅ — tolerant `<pick>N</pick>` parser; ~$0.001 / query; lifts R@1 ~6 pp on the bench | `internal/recall/rerank.go` |
| FTS5 query sanitization | ✅ — natural-language queries with hyphens / colons / parens etc. wrap meta-char tokens in double quotes | `internal/recall/search.go` |
| `recall distill` — async Haiku pass extracting durable facts/runbooks from turns | ✅ — schema v7 `turns.distilled_at` cursor; `source='distilled'` decisions | `internal/distill` |
| Recent-topics inject section (user-turn excerpts as topic launchers) | ✅ — filters system-generated turns + dedup by 80-char prefix | `internal/inject` |
| PreCompact Claude Code hook (catches turns added before context compression) | ✅ | `dist/setup.sh` |
| `recall bench longmemeval` — retrieval benchmark + coordinate-descent tuner | ✅ — 50/450 dev/holdout split, sweeps `k` + per-channel weights, optional rerank pass | `internal/bench` |
| `make setup` — one-shot install (build + model + every detected tool's MCP/hooks/agent rule) | ✅ — marker-bracketed merges into `~/.codex/AGENTS.md` and `~/.claude/CLAUDE.md` preserve user content | `dist/setup.sh` |
| Codex / Cursor MCP registration documented + automated by `make setup` | ✅ | `dist/setup.sh`, `dist/codex-AGENTS.md`, `dist/cursor-user-rule.md` |
| CLI parity with MCP (`recall search` etc. exposed at the shell) | ✅ | `cmd/recall/main.go` |

Still deferred:

- **Cross-project decision escalation** (≥3 projects → global) — straightforward SQL trigger or scheduled scan; not yet wired.
- **Codex rollout-file parser**: `threads.rollout_path` points at per-turn JSON rollout content; not yet parsed (we ingest the first user message, which is sufficient for session-level recall but loses per-turn detail in CLI sessions).
- **Pruning of raw turns after consolidation**: `recall consolidate` rewrites summaries but never deletes raw turns. A later opt-in `--prune-raw-after Nd` would compress storage further. (The decision store now self-prunes via `recall maintain`; raw turns don't yet.)
- **Spreading activation in `recall_search` / semantic search**: spread is currently injected into the SessionStart block via file co-occurrence; the FTS / cosine-ranked tools don't yet expand via edges.
- **Forgetting-curve consolidation**: low-salience decisions auto-summarised into a parent decision after N days of non-use. Partially built: `recall maintain`'s decay pass *deletes* stale low-salience auto-extracted decisions (conservative source allowlist) rather than summarising them into a parent — the summarise-into-parent variant is still unbuilt.
- **Session-level embedding channel**: tested as a 5th RRF channel; net regression on LongMemEval held-out (98.2% vs 98.9%). Abandoned for this dataset; might revisit for long real-world sessions where the answer spans many turns.

## Mental model: three tiers

Mirrors how human memory actually works.

| Tier | Lifetime | Source | Example |
|------|----------|--------|---------|
| **Episodic** | ~30d full, then summarized | Auto from session JSONL/SQLite | "On 2026-04-12 we touched `auth.go:142` and ran `go test ./...`" |
| **Semantic** | Permanent until contradicted | Curated + auto-promoted | "Org is `acme-corp`; CI lives in `ci-workflows`" |
| **Procedural** | Permanent, decays without use | Pattern-mined from repeated behavior | "User prefers integration tests over mocks; bundles small refactors into one PR" |

Episodic is *what happened*. Semantic is *what's true*. Procedural is *how we work*. All three are populated automatically; the user can curate semantic entries explicitly via `recall decide` and pin items for the active session via `recall pin`.

## Memory properties

These are the design moves that distinguish recall from a plain session search-index:

1. **Salience-weighted ranking.** Every memory has a salience score. Boost on: contradictions, corrections, errors, reverted edits, repeated mentions across sessions, explicit user confirmations. Decay on: age, non-use. Retrieval ranks by `relevance × salience`, not relevance alone.

2. **Associative recall (spreading activation).** Don't just text-match the current prompt. Build a co-occurrence graph over `(file, session, decision, project, tool)`. Top-K text matches seed activation; pull in their neighbors weighted by edge strength. Result: asking about `auth.go` surfaces the migration decision made two weeks ago in the same session as `auth_test.go`, even if the prompt never mentions migrations.

3. **Consolidation pipeline.** A nightly launchd job summarizes episodic data older than N days into compact procedural/semantic entries, then prunes verbatim turns. Working set stays small; long-term memory remains rich.

4. **Surprise capture.** The highest-signal events are errors, contradictions, and reverted edits — these are exactly what humans remember best. Hooks on `PostToolUse` (bash exit≠0, edit-then-revert within 10 min) and pattern detection on user turns ("no, actually…", "stop doing X", "let's go with Y instead") create high-salience entries automatically.

5. **Reconsolidation, not append.** When a new decision contradicts an old one ("actually use Postgres not SQLite"), version the existing entry rather than appending a new contradictory row. The system always knows the *current* belief; history is preserved but not in the hot path.

6. **Forgetting curve.** Episodic detail expires by default; salience escalates an entry to semantic before expiry. Mundane stuff fades; important stuff sticks. Configurable per-tier TTL.

7. **Cross-project escalation.** Same feedback given to N≥3 different projects → auto-promote to global scope. ("Don't mock the database in tests" said in three repos → global procedural memory.)

8. **Integration with existing `MEMORY.md`.** Treat the user's curated `~/.claude/projects/-Users-z/memory/` as the canonical semantic surface — the source of truth for human-approved facts. recall:
   - Reads it on every recall (highest-priority results).
   - *Suggests* new entries via a slash command surface; never auto-writes to it.
   - Detects staleness ("memory says file X exists; it doesn't") and tags for review.

## Architecture

```
sources (read-only)                         outputs
┌─────────────────────────────┐             ┌──────────────┐
│ ~/.claude/projects/*.jsonl  │──┐          │ CLI: recall  │
│ ~/.codex/state_5.sqlite     │──┼─►normalize│ MCP server   │
│ Cursor workspaceStorage/    │──┘          │ hooks/rules  │
│ MEMORY.md (semantic)        │             └──────────────┘
└─────────────────────────────┘                    ▲
              │                                    │
              ▼                                    │
   ┌─────────────────────────────────────┐         │
   │ ~/.local/share/recall/db.sqlite│────────┘
   │  • FTS5 over turns + summaries       │
   │  • salience scores, decay timers     │
   │  • co-occurrence graph (edges table) │
   │  • content-hashed idempotency        │
   └─────────────────────────────────────┘
              ▲
              │ writes from:
              │ ┌─ Claude Code Stop hook (real-time, primary)
              ├─┤ recall watch (poll every 30s, fallback + Codex/Cursor)
              │ └─ Surprise hooks (PostToolUse on errors/reverts)
              ▼
       launchd: nightly consolidation
```

**Dual ingestion** is critical for reliability: hook is fast and primary, watch is slow and authoritative. If the hook fails, the watch daemon catches it within 30 seconds. Ingestion is content-hash-keyed so double-writes are no-ops.

## Schema (SQLite, WAL mode, current version 9)

```sql
sources(id, kind, path, last_ingested_at)

sessions(id, source_id, project_dir, git_branch, source_version,
         started_at, ended_at, summary,
         summary_consolidated_at, summary_source,   -- v3
         turn_count, salience)

turns(uuid PK, session_id, parent_uuid, idx, role, ts, text, has_tool_use,
      embedding BLOB,                                -- v2
      distilled_at INTEGER)                          -- v7

files(id, session_id, turn_uuid, project_dir, path, op, ts)

decisions(id, session_id, project_dir, ts, kind, text,
          source,                -- 'pattern' | 'tool' | 'cli' | 'user-memory-md'
                                 -- | 'surprise' | 'distilled'   -- v7
          superseded_by,         -- reconsolidation chain
          salience, use_count, last_used_at,
          confidence,            -- v8 trust score ∈ [0,1], default 0.5
          embedding BLOB)        -- v2

decision_feedback(id, decision_id, kind, ts, weight, note)  -- v8
          -- kind: 'confirmed' | 'contradicted' | 'stale'; append-only audit

decision_paraphrases(id, decision_id, text, embedding BLOB, ts)  -- v5

edges(src_kind, src_id, dst_kind, dst_id, weight)   -- co-occurrence graph

pins(id, session_id, project_dir, ts, text, cleared_at)   -- v4

tool_events(id, ts, cwd, tool, file_path, before_hash, after_hash)  -- v2

entities(id, name, display, first_seen, last_seen, mention_count)   -- v6
entity_mentions(entity_id, source_kind, source_id, ts,              -- v6
                PRIMARY KEY (entity_id, source_kind, source_id))

entity_cards(entity_id PK, summary, embedding BLOB,                 -- v9
             source_count, refreshed_at)   -- auto-curated entity wiki

ingest_state(source_id, key, last_offset, last_line, last_uuid, updated_at)

turn_fts USING fts5(text, content='turns', tokenize='porter unicode61')

schema_version(version PK)
```

Salience updates run incrementally on read (lazy decay via SQL expression) + on every MCP / inject surfacing (use_count bump). Decisions are versioned via `superseded_by`; old rows stay queryable but rank below current ones. Embedding columns are populated by `recall embed`; queries that don't use them simply ignore them. Entity mentions are extracted on insert (regex over text, normalized lowercase keys) — no async distillation pass needed for the basic `--entity` filter to work.

## Reliability invariants

The system must hold these without exception:

- **Idempotent.** Running `recall ingest` twice produces the same DB state. Keyed on `(source, session_id, idx, content_hash)`.
- **Atomic.** All multi-row writes in a single SQLite transaction. WAL mode for crash safety.
- **Schema-drift tolerant.** Adapters detect upstream schema versions. Unknown event types log a warning and skip — never crash.
- **Dual-source.** Every adapter has hook *and* watch path. Either alone produces a complete DB.
- **Self-healing.** `recall doctor` checks schema, file permissions, hook installation, watch daemon health, and reports gaps. Auto-runs on every CLI invocation; fixes what it can.
- **Atomic external writes.** Any file we write outside our DB (CLAUDE.md edits, `.cursor/rules/00-recall.mdc`, exports) uses write-temp-then-rename.
- **Migrations.** Schema version pinned; auto-migrate forward; never destructive.
- **Backups.** `recall export --markdown` produces a git-friendly snapshot. Run nightly.
- **Tests.** Property-based tests over a golden corpus of session JSONL/SQLite snapshots committed to the repo.

## Seamlessness invariants

What the user experiences:

- **Zero daily commands.** Install once. After that: never type `recall foo`. Hooks + MCP tools + auto-injection do everything.
- **Never blocks.** Ingestion is async; hooks fall through to the watch daemon if exceeded budget.
- **Single user-facing binary** + a downloaded ONNX Runtime dylib + a tokenizer-and-model bundle, all SHA256-pinned. Apple NLEmbedding works zero-config as the fallback before `recall download-model` runs.
- **Cross-tool by default.** Same DB, same MCP server, same memories, used by Claude Code / Codex / Cursor without per-tool curation.
- **Confirmation, not interruption.** When a new decision is auto-extracted with low confidence, the next session start surfaces a one-line confirmation rather than asking mid-flow.
- **Observable.** `recall stats` and `recall doctor` show DB health, asset state, hook installation, MCP registration. The user can see it's working without reading code.
- **Reversible.** `recall forget --decision <id>`, `recall unpin <id>`, `recall install-model --force` always work.

## Per-tool integration

| Tool | Ingestion | recall surface | Auto-injection |
|------|-----------|----------------|----------------|
| Claude Code | `Stop` hook (primary, ~5 ms incremental) + `PreCompact` hook (catches turns added between the last Stop and a context-compaction event) + watch (fallback) + `PostToolUse` Bash matcher (records bash failures + edit/revert hash pairs) | MCP server registered globally via `~/.claude.json` (Claude Code 2.1+ canonical store, written by `claude mcp add`); `~/.claude/CLAUDE.md` (marker-bracketed by `dist/setup.sh`) instructs the agent to call `recall_*` proactively, especially `recall_search` for "what is X / where is X" questions | `SessionStart` hook prepends a 1500-token block: pins, instructions (procedural decisions), decisions w/ stable IDs, recent topics (user-turn excerpts), recent sessions, recent files, co-occurring file neighbors, "this session so far" |
| Codex | watch daemon polling `~/.codex/state_5.sqlite` `threads` table (CLI / desktop's bundled CLI). Codex *desktop's* GUI sessions are server-side at OpenAI and not locally ingestable — only MCP read/write works for those | MCP server in `~/.codex/config.toml` (registered via `codex mcp add`); `~/.codex/AGENTS.md` (marker-bracketed) carries the same proactive-recall rule | n/a (no SessionStart hook) — relies on AGENTS.md + MCP. Agent calls `recall_summary` at task start per the rule |
| Cursor | watch daemon polling `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` (`composer.composerHeaders` + `cursorDiskKV.bubbleId:<cid>:<bid>` for per-turn content) | MCP server in `~/.cursor/mcp.json` (file-based; merged-not-clobbered by setup.sh). User Rules — the global agent-prompt tier — are server-side / Settings-UI-only on Cursor's side; we ship `dist/cursor-user-rule.md` for the user to paste once. Workspace-scoped `.cursor/rules/*.mdc` is file-based but per-repo. | n/a (no SessionStart hook). Per-turn agent behavior driven by the user rule + MCP |

## CLI surface

Day-to-day commands (none required for the seamless path; hooks + MCP do the work):

- `recall stats` — DB counts + date range
- `recall doctor` — health check (binary, PATH, DB, contents, ONNX assets, Claude Code + Hermes wiring, CLAUDE.md, log dir)
- `recall search "<query>" [--project DIR|all] [--limit N] [--json]` — FTS5 over turns
- `recall ingest [--source all|claude-code|codex|cursor|user-memory] [--root PATH]` — incremental, idempotent
- `recall inject [--project DIR] [--prompt-stdin] [--budget N] [--days N]` — render the recall block
- `recall decide "<text>" [--kind fact|preference|feedback] [--project DIR|-] [--salience N]` — record a decision; auto-runs reconsolidation
- `recall decisions [--project DIR|all] [--kind K] [--limit N] [--json]` — list active decisions
- `recall forget --decision <id> | --pattern-text "<text>"` — soft-delete
- `recall pin "<text>" [--session SID] [--project DIR|-]` — pin for session/project
- `recall unpin <id>` / `recall pins [--session SID|--project DIR|all]`
- `recall embed [--provider onnx|apple] [--scope decisions|turns] [--batch N]` — fill missing embeddings
- `recall download-model [--force]` — fetch ONNX Runtime + sentence-transformer (SHA256-pinned)
- `recall install-model --from <dir>` — airgap / mirror side-load
- `recall consolidate [--days N] [--max N] [--model M]` — Haiku rewrite of old session summaries (opt-in, `ANTHROPIC_API_KEY` required)
- `recall paraphrase [--limit N] [--per-decision K]` — Haiku-generated alt-phrasings, embedded for semantic recall (opt-in)
- `recall distill [--days N] [--batch N] [--max-turns N] [--dry-run]` — async Haiku pass that extracts durable facts / runbooks / preferences from turns and writes them as decisions with `source='distilled'`. Idempotent via `turns.distilled_at` + `InsertDecisionIfNew` dedup. Opt-in.
- `recall search "<query>" [--mode hybrid|lexical|semantic] [--entity NAME] [...]` — see recall surface below
- `recall bench longmemeval <data.json> [--tune] [--rerank] [...]` — retrieval benchmark + coordinate-descent tuner. Used for regression-testing retrieval changes; reports R@1/R@5/R@10 against a held-out split.
- `recall record-tool-event` — reads PostToolUse JSON from stdin (Bash-failure / edit-revert capture)
- `recall maintain [--project DIR] [--no-dedup] [--no-decay] [--dedup-threshold C] [--decay-floor S] [--decay-days N] [--dry-run] [--json]` — housekeeping: merge semantic near-duplicate decisions and age out stale low-salience auto-extracted rows. Runs automatically once a day inside `recall watch`
- `recall wiki [--show NAME | --list] [--min-mentions N] [--max N] [--dry-run] [--json]` — build / show / list the auto-curated entity wiki. Building Haiku-distills a card per @entity from its mentions (opt-in, `ANTHROPIC_API_KEY`); `--show` / `--list` are local-only
- `recall watch [--interval 30s]` — poll all sources on a timer; also runs `recall maintain` once per 24h (gated via an `ingest_state` marker)
- `recall mcp` — run the stdio MCP server (invoked by editors)

## MCP surface

Tools exposed over stdio MCP, callable from any editor that registers the server:

- `recall_summary(project, days?, budget?)` — compact recall block as markdown (pins, instructions, decisions, recent topics, recent sessions/files, co-occurrence neighbours)
- `recall_search(query, mode?, project?, limit?, hyde?, entity?)` — hybrid retrieval: FTS5 lexical + cosine semantic + temporal recency, fused via Reciprocal Rank Fusion. Default `mode='hybrid'`. `entity` constrains the candidate pool to items mentioning `@<entity>`.
- `recall_semantic_search(query, project?, limit?)` — cosine similarity only over decisions; takes max cosine across canonical + paraphrase embeddings per decision; HyDE-expanded query when `ANTHROPIC_API_KEY` is set
- `recall_decisions(project, kind?, limit?)` — active decisions w/ salience-weighted ranking; pins surface here
- `recall_files(project, days?, limit?)` — recently touched files
- `recall_sessions(project?, days?, limit?)` — recent sessions w/ summaries
- `record_decision(text, kind?, project?)` — agent-side durable capture (auto-reconsolidation: cosine ≥0.85 supersedes; 0.65–0.85 surfaces as "related")
- `pin_for_session(text, session_id?, project?)` — agent-side "remember for this session" pin
- `recall_wiki(entity)` — fetch the auto-curated reference card for an @entity (rolled-up summary across all sessions, built by `recall wiki`). Returns found=false when the entity has no card yet

## Key design decisions

- **Pure-Go static binary.** Go 1.26 + `modernc.org/sqlite` (no cgo for SQLite). The only cgo is the ONNX bridge and Apple `NLEmbedding`.
- **Local embeddings, no cloud at query time.** ONNX `all-MiniLM-L6-v2` (q8) via Microsoft's ONNX Runtime through our own ~500 LOC CGO bridge (vendored MIT headers only — no third-party Go ONNX binding); Apple `NLEmbedding` as the zero-config macOS fallback, Ollama elsewhere. Pure-Go cosine (no `sqlite-vec`).
- **No third-party MCP / tokenizer / ONNX-binding libraries on the agent-facing path.** The MCP wire protocol (line-delimited JSON-RPC 2.0, four methods) is implemented in stdlib (~250 LOC); the BERT WordPiece tokenizer is hand-rolled (~210 LOC). Minimises supply-chain exposure.
- **Hybrid retrieval via RRF.** FTS5 lexical + cosine (decision + paraphrase embeddings, optional HyDE) + temporal recency, fused with tunable per-channel weights; the bench adds a content-word-overlap channel + optional Haiku rerank. `recall_search` defaults to all channels; `lexical`/`semantic` are escape hatches.
- **Trust scoring (v8).** `confidence ∈ [0,1]` multiplies `EffectiveSalienceExpr` (neutral at the 0.5 default). A near-verbatim re-assertion (cosine ≥0.92) reinforces an existing fact; a 0.85–0.92 match supersedes it and lowers its confidence. Limitation: cosine can't separate "reworded" from "contradicted", so reinforce is deliberately conservative; reliable paraphrase-level merging needs an LLM classifier (future, opt-in).
- **Push-once, pull-thereafter.** Inject runs only at SessionStart (~1500 tokens); mid-session recall is the agent calling the MCP tools. Per-prompt re-injection was tried and removed for token economics.
- **Per-tool MCP registration** under the server id `recall`: Claude Code `~/.claude.json` (`claude mcp add`), Codex `~/.codex/config.toml`, Cursor `~/.cursor/mcp.json`, Hermes `~/.hermes/config.yaml`. `recall doctor` verifies registration.

## Non-goals

- No cloud sync. (Markdown export covers it; it's deferred.)
- No personality system. (Orthogonal to memory; lives in CLAUDE.md.)
- No write-back to source agents' DBs. (Read-only, always.)
- No daemon required for Claude Code (the Stop hook covers it). Watch daemon is opt-in for Codex / Cursor and as a fallback for Claude Code.
- No always-on per-prompt re-injection. SessionStart inject + agent-pull via MCP tools is the model; UserPromptSubmit was tried and removed for token economics.
- No third-party MCP, tokenizer, or ONNX-binding libraries on the recall hot path. Stdlib + vendored MIT headers + code we wrote.
