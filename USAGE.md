# Usage — recall

Comprehensive reference for every CLI command and MCP tool the `recall` binary exposes. Browse the table for a quick map, then jump to the detail section for each.

For the design rationale behind these surfaces see [DESIGN.md](./DESIGN.md). For the security inventory see [SBOM.md](./SBOM.md).

---

## Quick reference — CLI

| Command | One-line | When to use |
|---|---|---|
| [`recall ingest`](#recall-ingest) | Read source agents' session storage into the DB | Idempotent; fires automatically from the Stop hook. Run manually for backfill. |
| [`recall inject`](#recall-inject) | Render the `<recall>` block | Wired into SessionStart hook. Manual: preview what the agent will see. |
| [`recall search`](#recall-search) | Hybrid / lexical / semantic search over turns + decisions | Default tool for "find me anything matching X". |
| [`recall stats`](#recall-stats) | DB row counts + date range | Quick health snapshot. |
| [`recall doctor`](#recall-doctor) | Self-check (8 probes, +Hermes when installed) | Run after install / upgrade / config change. |
| [`recall mcp`](#recall-mcp) | Stdio MCP server | Spawned by Claude Code / Codex / Cursor; not run by hand. |
| [`recall decide`](#recall-decide) | Record a decision (fact / preference / feedback / instruction) | Capture a durable rule from the shell. |
| [`recall decisions`](#recall-decisions) | List active decisions for a project | Audit what's stored. |
| [`recall forget`](#recall-forget) | Soft-delete a decision | When a captured decision is wrong / stale. |
| [`recall maintain`](#recall-maintain) | Merge duplicate decisions + age out stale ones | Periodic housekeeping; runs daily inside `recall watch`. |
| [`recall wiki`](#recall-wiki) | Build / show the auto-curated entity wiki | Roll up everything known about an @entity into one card. Build requires `ANTHROPIC_API_KEY`. |
| [`recall pin`](#recall-pin) | Pin a note for the rest of the session / project | Hard-prioritise a note in every inject. |
| [`recall unpin`](#recall-unpin) | Clear a pin | After a session-pin's purpose is fulfilled. |
| [`recall pins`](#recall-pins) | List active pins | Audit what's pinned. |
| [`recall download-model`](#recall-download-model) | Fetch ONNX Runtime + sentence-transformer (SHA256-pinned) | One-time per machine for the high-quality semantic recall path. |
| [`recall install-model --from DIR`](#recall-install-model) | Side-load model assets from a local directory | Airgap / mirror installs in security-controlled orgs. |
| [`recall embed`](#recall-embed) | Populate embeddings on decisions / turns | After a fresh DB or after `download-model`. |
| [`recall paraphrase`](#recall-paraphrase) | Generate alt-phrasings per decision via Haiku | Improves semantic recall on diverse query phrasings. Requires `ANTHROPIC_API_KEY`. |
| [`recall consolidate`](#recall-consolidate) | Rewrite old session summaries via Haiku | Periodic; tightens session titles. Requires `ANTHROPIC_API_KEY`. |
| [`recall distill`](#recall-distill) | Extract durable facts / runbooks from recent turns into decisions | Periodic; turns conversation context into searchable decisions. Requires `ANTHROPIC_API_KEY`. |
| [`recall bench longmemeval`](#recall-bench-longmemeval) | Run the LongMemEval retrieval benchmark with optional tuner | Validate retrieval quality after a change; produces R@1/R@5/R@10 against a held-out split. |
| [`recall record-tool-event`](#recall-record-tool-event) | Capture a PostToolUse hook event | Wired automatically; not run by hand. |
| [`recall watch`](#recall-watch) | Poll loop over Codex / Cursor / Hermes + daily maintenance | Run via launchd if you use those tools. |

## Quick reference — MCP

The `recall` MCP server (registered via `claude mcp add --scope user recall ~/.local/bin/recall mcp`) exposes:

| Tool | One-line |
|---|---|
| [`recall_summary`](#recall_summary) | Compact recall block as markdown for any project |
| [`recall_search`](#recall_search) | **Default** hybrid search (RRF over FTS5 + semantic) over turns + decisions |
| [`recall_semantic_search`](#recall_semantic_search) | Cosine-only over decisions, optionally HyDE-expanded |
| [`recall_decisions`](#recall_decisions) | List active decisions for a project, salience-ranked |
| [`recall_files`](#recall_files) | Recently touched files |
| [`recall_sessions`](#recall_sessions) | Recent sessions with summaries |
| [`record_decision`](#record_decision) | Agent-side durable capture (reinforces / supersedes related facts) |
| [`pin_for_session`](#pin_for_session) | Agent-side "remember for this session" pin |
| [`recall_wiki`](#recall_wiki) | Reference card for an `@entity` (rolled-up summary across sessions) |

---

## Setup

End-to-end install. Each step is idempotent — re-running is safe.

### One-shot bootstrap (recommended)

```bash
git clone git@github.com:zdaniels/recall.git
cd recall
make setup        # or: bash dist/setup.sh
```

`dist/setup.sh` builds + installs `recall`, downloads the ONNX model with SHA256 verification, wires every supported tool found on this machine (Claude Code MCP + hooks; Codex MCP + AGENTS.md; Cursor `mcp.json`; Hermes `~/.hermes/config.yaml`), backfills existing history, embeds decisions, runs paraphrase if `ANTHROPIC_API_KEY` is set, installs + loads the launchd watch daemon, and finishes with `recall doctor`. The only manual step it can't automate is the Cursor User Rule paste (Cursor's user-rules tier is server-side / UI-only) — the script prints the path to the file you paste from.

**What the script preserves on machines with prior config:**

- `~/.claude.json` (MCP) — `claude mcp add` skipped if `recall` is already registered. Other servers untouched.
- `~/.claude/settings.json` (hooks) — for each event (`SessionStart`, `PostToolUse`, `Stop`), our hook entry is **appended** to the existing array iff no entry with the same command is already present. Other top-level keys, other events (e.g. `Notification`), and unrelated hooks under our events are preserved.
- `~/.codex/config.toml` (MCP) — `codex mcp add` skipped if `recall` is already registered.
- `~/.codex/AGENTS.md` — our content is bracketed with `<!-- agent-memory:begin -->` / `<!-- agent-memory:end -->` markers; re-runs replace only the content between them. If the file exists with custom content and no markers, the script leaves it alone and prints how to integrate. Files written by older script versions (no markers) get auto-migrated when their content matches the current `dist/codex-AGENTS.md` exactly.
- `~/.cursor/mcp.json` — `jq` sets only `.mcpServers["recall"]`; other MCP servers and any existing top-level keys survive.

The launchd plist (`~/Library/LaunchAgents/ai.fantazm.recall.watch.plist`) is rendered fresh from `dist/ai.fantazm.recall.watch.plist.template` each run — only an issue if you'd hand-tuned that plist.

If you'd rather see what each step does, the manual walkthrough below is the same flow split out.



### 1. Prerequisites

- macOS (arm64), Linux (x64 / arm64), or Windows (x64).
- At least one supported agent (Claude Code, Codex, Cursor, Hermes, …) — optional if you only want the CLI.
- No Go toolchain needed for the prebuilt binary; building from source needs Go 1.26+ and a C compiler (the ONNX bridge is cgo on every platform).

### 2. Install the binary

Use the installer from the [README](./README.md#install) (`curl … | sh`, or `irm … | iex` on Windows — it downloads the signed per-platform archive, verifies SHA256, and drops the binary at `~/.local/bin/recall`). Or build from source:

```bash
git clone https://github.com/zdaniels/recall.git
cd recall
make install     # compiles + installs ~/.local/bin/recall
```

`make release` produces the same per-platform tarball + SHA256 the GitHub Actions workflow publishes.

### 3. Download the ONNX semantic-recall model

The default semantic recall path uses `sentence-transformers/all-MiniLM-L6-v2` (q8) running through ONNX Runtime. Total ~57 MiB. Every artifact is SHA256-pinned in `internal/embed/assets.go` and re-verified on every load — a hash mismatch aborts and removes the file.

```bash
recall download-model
```

This fetches:

| Artifact | Size | Source |
|---|---|---|
| ONNX Runtime (`.dylib` / `.so` / `.dll`, per OS) | ~35 MiB | `github.com/microsoft/onnxruntime/releases` (v1.20.1) |
| `model_qint8_arm64.onnx` (arch-portable; the name is HF's export label) | ~22 MiB | `huggingface.co/sentence-transformers/all-MiniLM-L6-v2` |
| `tokenizer.json` + `config.json` | <1 MiB | same |

Files land under `~/.local/share/recall/{runtime,models}/`. Re-running is a no-op when hashes already match.

**Airgap / mirror install** — copy the four artifacts to a directory on the target machine and run `recall install-model --from <dir>` instead. Same SHA256 verification. See [SBOM.md § Airgap install](./SBOM.md#airgap--mirror-install) for the full mirror procedure.

**Skip the download?** Without ONNX assets, the embedder falls back to Apple `NLEmbedding` (built into macOS, zero disk cost, lower quality). `recall doctor` will warn but everything still works.

### 4. Wire Claude Code (manual)

`dist/setup.sh` does all of this automatically. By hand:

```bash
claude mcp add --scope user recall ~/.local/bin/recall mcp   # writes ~/.claude.json (the canonical MCP store in Claude Code 2.1+)
recall ingest && recall embed --scope decisions              # backfill history + embed decisions
```

Then add the lifecycle hooks to `~/.claude/settings.json` (merge with any existing `hooks`):

```jsonc
{
  "hooks": {
    "SessionStart": [{ "hooks": [{ "type": "command", "command": "recall inject --prompt-stdin --budget 1500 2>/dev/null" }] }],
    "PostToolUse":  [{ "matcher": "Bash", "hooks": [{ "type": "command", "command": "recall record-tool-event 2>>~/.local/state/recall/hook.err" }] }],
    "Stop":         [{ "hooks": [{ "type": "command", "command": "mkdir -p ~/.local/state/recall && recall ingest >> ~/.local/state/recall/ingest.log 2>&1" }] }]
  }
}
```

SessionStart injects the `<recall>` block; PostToolUse(Bash) captures tool failures + edit/revert pairs; Stop ingests each new turn. Codex, Cursor, and Hermes are wired in [Cross-tool integration](#cross-tool-integration-codex-cursor--hermes) below.

### 5. (Optional) Anthropic API key + watch daemon

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # enables recall paraphrase / distill / consolidate / HyDE; semantic search works without it
make watch-install && launchctl load ~/Library/LaunchAgents/ai.fantazm.recall.watch.plist   # polls Codex/Cursor/Hermes + runs daily maintenance
```

### 6. Verify

```bash
recall doctor       # all probes ✓
recall stats        # row counts + date range
```

Open a new Claude Code session in a project directory — you should see a `<recall>` block at the top of the assistant's first message.

### Managed-fleet install (Kandji, Jamf, etc.) — zero secondary egress

For managed-device fleets where the install path can only reach your org's own GitHub (no `huggingface.co`, no `github.com/microsoft/...`), use the **`-full` release artifact**. Each release ships two tarballs:

| Artifact | Size | Contains | Use when |
|---|---|---|---|
| `recall-vX.Y.Z-darwin-arm64.tar.gz` | ~4 MiB | binary only | Personal install, your machine reaches HF and microsoft.com freely |
| `recall-vX.Y.Z-darwin-arm64-full.tar.gz` | ~62 MiB | binary + ONNX runtime + sentence-transformer + tokenizer + config | Managed-fleet install, security tooling blocks the secondary downloads |

Tarball layout for `-full`:

```
recall                                # binary
runtime/libonnxruntime.dylib          # ~35 MiB
models/all-MiniLM-L6-v2/
  model_qint8_arm64.onnx              # ~22 MiB
  tokenizer.json                      # ~455 KiB
  config.json                         # ~612 B
```

Both artifacts are SHA256-pinned. Same hashes the binary verifies at load time. Reproducible build: the release workflow uses `recall download-model` itself to populate the bundle, going through the same pinned-hash code path users hit.

A managed-fleet installer (whatever MDM you're using — Kandji Custom Script, Jamf script policy, etc.) should:
1. Fetch the `-full` artifact + its `.sha256` sidecar from the GitHub releases API (Contents:Read PAT for private repos).
2. Verify the SHA256.
3. Extract: `recall` → `~/.local/bin/`, `runtime/` and `models/` → `~/.local/share/recall/`.
4. Fetch the source archive at the matching tag and run `dist/setup.sh` — it detects the pre-staged pinned files and skips the secondary egress.

Result: zero `huggingface.co` / `microsoft.com` requests from the managed Mac at install time.

---

## Cross-tool integration: Codex, Cursor + Hermes

Setup above wires Claude Code. The MCP server, decision/pin store, and FTS5+semantic indexes are shared — wiring Codex and Cursor lets the agent inside those tools call the same `recall_*` tools and read your accumulated memory.

### Codex (CLI or desktop app)

The Codex desktop app bundles the Codex CLI at `/Applications/Codex.app/Contents/Resources/codex`; both share `~/.codex/config.toml`.

```bash
# 1. Make `codex` reachable on PATH (if you only have the desktop app):
ln -sf /Applications/Codex.app/Contents/Resources/codex ~/.local/bin/codex
codex --version

# 2. Register the MCP server (writes ~/.codex/config.toml):
codex mcp add recall ~/.local/bin/recall mcp
codex mcp list   # recall should appear

# 3. Drop the always-on agent rule into ~/.codex/AGENTS.md:
cp dist/codex-AGENTS.md ~/.codex/AGENTS.md
```

Restart the Codex desktop app once so the new config is picked up.

**Heads-up — transcripts**: the Codex *CLI* writes session rollouts under `~/.codex/` and the watch daemon ingests them automatically. The Codex *desktop app* is a thin client that keeps conversation history server-side at OpenAI; the watch daemon has nothing to ingest from it. MCP read/write still works in both — what's lost is full-transcript ingestion of desktop sessions.

### Cursor

Cursor's user-global rules are synced through your Cursor account and configured via the UI; there is no on-disk file path you can write to that activates them globally. Workspace-scoped rules at `<repo>/.cursor/rules/*.mdc` are file-based.

```bash
# 1. Register the MCP server globally (~/.cursor/mcp.json):
cat > ~/.cursor/mcp.json <<'JSON'
{
  "mcpServers": {
    "recall": {
      "command": "/Users/$USER/.local/bin/recall",
      "args": ["mcp"]
    }
  }
}
JSON

# 2. Open Cursor → Settings → Rules → User Rules and paste the body of
#    dist/cursor-user-rule.md (the fenced block under the heading).
#    This is a one-time UI step — Cursor's user-rules tier is server-side,
#    not file-based.

# 3. Restart Cursor.
```

If you'd rather keep rules in version control, drop `dist/cursor-user-rule.md`'s body into `<repo>/.cursor/rules/recall.mdc` with `alwaysApply: true` frontmatter — that's workspace-scoped but file-based.

The Cursor adapter ingests session headers + per-bubble turn content via the `bubbleId:<composerId>:<bubbleId>` rows in `cursorDiskKV` (mapped: `type=1`→user, `type=2`→assistant, `createdAt`→ts). Composer headers in `composer.composerHeaders` give summaries and timestamps. The watch daemon polls every 30 s; or run `recall ingest --source cursor` manually.

### Hermes (NousResearch)

Hermes is **bidirectional**: recall already ingests Hermes sessions from `~/.hermes/state.db` (canonical SQLite store, FTS5 + WAL — `sessions` + `messages` tables, timestamps as unix-second floats), and it also registers itself as an MCP server inside Hermes so Hermes can query the shared store.

```yaml
# 1. Register the MCP server in ~/.hermes/config.yaml (merged by `make setup`,
#    or add by hand under the mcp_servers: key):
mcp_servers:
  recall:
    command: /Users/$USER/.local/bin/recall
    args: [mcp]
```

```bash
# 2. Add the body of dist/hermes-rule.md to your Hermes system prompt or a
#    skill so Hermes calls recall_* proactively.
# 3. Reload MCP servers inside Hermes:
/reload-mcp        # (or restart `hermes chat`)
```

`make setup` performs step 1 automatically when `~/.hermes/config.yaml` exists, using `yq` to merge (it preserves your comments + key order; falls back to printing the snippet if `yq` isn't installed). It never creates a partial config — if Hermes hasn't been run yet, run it once so it generates `config.yaml`, then re-run setup. The Hermes adapter ingestion runs on the same 30 s watch poll; or run `recall ingest --source hermes` manually.

### Verifying cross-tool integration

In Codex or Cursor, ask any non-trivial coding question *without* mentioning memory. The agent should call `recall_summary` (or `recall_search`) before answering — visible as a tool call in the UI's transcript. If it doesn't, the rule isn't loaded:

- **Codex**: check `~/.codex/AGENTS.md` exists and the file's content matches `dist/codex-AGENTS.md`.
- **Cursor**: re-paste the user rule into Settings → Rules → User Rules. Be sure you saved.
- **Hermes**: run `recall doctor` (it probes `~/.hermes/config.yaml` for the `recall` entry when Hermes is installed); confirm `dist/hermes-rule.md` is in your system prompt / a skill, then `/reload-mcp`.

End-to-end smoke test (run after using all three tools): `recall stats` — sessions/turns counts should reflect Claude Code, Codex CLI, and Cursor activity.

---

## Common workflows

### Fresh install on a new Mac

See [Setup](#setup) above for the full walkthrough. The condensed flow:

```bash
git clone git@github.com:zdaniels/recall.git && cd recall
make install
recall download-model
claude mcp add --scope user recall ~/.local/bin/recall mcp
# add the hooks block to ~/.claude/settings.json (step 5 above)
recall ingest && recall embed --scope decisions
recall doctor
```

### Daily use (zero manual commands)

The hooks + MCP do the work. SessionStart inject prepends recall. Stop hook ingests new turns. Mid-session, Claude calls `recall_*` MCP tools when needed.

### Capture a runbook from the shell

```bash
recall decide "to deploy xcs-web-app, push to main and the GHA pipeline handles it" --kind instruction
```

Next session, this surfaces in the Instructions section of the inject block.

### Pin a goal for the day

```bash
recall pin "goal: ship the auth migration before friday" --session "$CLAUDE_SESSION_ID"
# OR project-scoped:
recall pin "goal: ship the auth migration before friday"
```

### Remove a pin you no longer need

```bash
recall pins                # find the id
recall unpin 7
```

### Audit what's stored for the current project

```bash
recall stats
recall decisions
recall pins
recall search "auth migration" --mode semantic
```

### Airgap / mirror install

```bash
# On a machine with internet:
recall download-model
cp -r ~/.local/share/recall/{runtime,models} /shared/recall-assets/

# On the airgapped machine:
recall install-model --from /shared/recall-assets/    # SHA256-verified
```

---

## CLI reference

### `recall ingest`

Read source agents' session storage into the local DB. Idempotent — re-runs are no-ops on data already ingested.

```
recall ingest [--source all|claude-code|codex|cursor|user-memory] [--root PATH] [--db PATH]
```

**Sources:**
- `all` (default) — every adapter
- `claude-code` — `~/.claude/projects/*.jsonl`
- `codex` — `~/.codex/state_5.sqlite` `threads` table
- `cursor` — `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`
- `user-memory` — `~/.claude/projects/*/memory/*.md` (curated entries)

**Examples:**

```bash
recall ingest                              # all sources
recall ingest --source claude-code         # only Claude Code transcripts
recall ingest --source codex --root /alt/path/to/codex/state.sqlite
```

Wired into the **Claude Code Stop hook** automatically — every assistant turn finishes, ingest fires.

### `recall inject`

Render the `<recall>` block: a **Ground truth** directive (telling the agent to treat the block as established and prefer it over re-deriving or re-asking), then pins, instructions, decisions (corroborated ones marked `✓`), recent sessions, recent files, co-occurring file neighbors, and "this session so far". Stays under a token budget; emits nothing for projects with no recorded activity.

```
recall inject [--project DIR] [--days N] [--budget N] [--prompt-stdin] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--project` | `$PWD` | Absolute path. With ancestor scoping (`/Users/z` decisions show in `/Users/z/Documents/sub-project`). |
| `--days` | `30` | Lookback window. |
| `--budget` | `250` | Approximate tokens. ~4 chars per token. |
| `--prompt-stdin` | off | Read JSON from stdin to extract `cwd` + `session_id` + `prompt`. Used by the SessionStart hook. |

**Examples:**

```bash
recall inject                                # render block for $PWD
recall inject --project /Users/z/Documents/recall --budget 1500
echo '{"cwd":"/proj","session_id":"abc"}' | recall inject --prompt-stdin
```

The **SessionStart hook** runs `recall inject --prompt-stdin --budget 1500`.

### `recall search`

Hybrid retrieval over turns (FTS5) and decisions (cosine semantic), with a temporal-recency channel re-ranking the union. Three channels fused via Reciprocal Rank Fusion. Modes:

- `hybrid` (default) — fuses **lexical FTS5 + cosine semantic + temporal recency** via RRF. Best ranking on most queries.
- `lexical` — FTS5 only, returns turns.
- `semantic` — cosine only, returns decisions; HyDE-expanded query when `ANTHROPIC_API_KEY` is set.

```
recall search QUERY... [--mode hybrid|lexical|semantic] [--project DIR|all] [--limit N] [--entity NAME] [--json] [--hyde|--no-hyde]
```

**Flags:**

- `--project DIR|all` — filter by project_dir (default `$PWD` with ancestor scope; `all` for global).
- `--entity NAME` — restrict to items mentioning `@<NAME>` (the leading `@` is optional). Resolves case-insensitively against the `entities` table; missing entities short-circuit to no hits.
- `--mode` — `hybrid` (default) | `lexical` | `semantic`.
- `--hyde` / `--no-hyde` — force HyDE on/off. Default: on when `ANTHROPIC_API_KEY` is set.

**Examples:**

```bash
recall search "executable installation directory"            # hybrid, default
recall search "auth migration" --mode lexical --limit 20
recall search "deploy procedure" --mode semantic --json
recall search "memory layout" --project all --limit 50      # cross-project
recall search "review feedback" --entity bob --limit 10      # only items mentioning @bob
```

Mirror MCP tool: [`recall_search`](#recall_search).

### `recall stats`

```
recall stats [--db PATH]
```

Output:

```
recall stats (db=/Users/z/.local/share/recall/db.sqlite)
  sessions: 21
  turns:    5908
  files:    484
  projects: 4
  range:    2026-04-17 → 2026-05-01
```

### `recall doctor`

Self-check. Reports `✓` (pass), `⚠` (recoverable warning), `✗` (failure).

```
recall doctor [--db PATH]
```

Probes: binary location/size, recall on PATH, DB file, DB contents, ONNX semantic recall assets, Claude Code wiring (hooks + MCP registration in `~/.claude.json`), global CLAUDE.md, log dir — plus a Hermes wiring probe that appears only when `~/.hermes/` exists (checks for the `recall` entry in `~/.hermes/config.yaml`).

### `recall mcp`

Run the stdio MCP server. **Not invoked manually** — Claude Code spawns it as a child process via the `recall` entry registered in `~/.claude.json`.

```
recall mcp [--db PATH]
```

Implements the JSON-RPC 2.0 MCP wire protocol against stdin/stdout (line-delimited). Stops on stdin EOF.

### `recall decide`

Record a durable decision from the shell. Auto-runs reconsolidation with trust scoring:

- **Re-assertion** — if the text is a near-verbatim restatement of an existing decision (cosine ≥ 0.92), that decision's **confidence** is raised instead of inserting a near-duplicate (prints `reinforced existing decision #N`). This is how a fact becomes "established" over time.
- **Contradiction / refinement** — moderately-similar decisions (cosine 0.85–0.92) are marked `superseded_by` the new one, and the superseded row's confidence is lowered.

Confidence ∈ [0,1] (default 0.5) feeds ranking as a 0.6–1.4× multiplier (neutral at 0.5) and surfaces as `✓` in the inject block once a fact crosses 0.7. Every change is logged to the `decision_feedback` audit table.

```
recall decide TEXT... [--kind fact|preference|feedback|instruction] [--project DIR|-] [--salience N] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--kind` | `fact` | `fact` = stable knowledge, `preference` = soft choice, `feedback` = correction / "don't X", `instruction` = procedural runbook (renders in its own inject section). |
| `--project` | `$PWD` | `-` for global scope. |
| `--salience` | `1.5` | Above pattern-extracted (0.5), below user-curated MEMORY.md (2.5). |

**Examples:**

```bash
recall decide "we use Postgres for new services" --kind preference
recall decide "to roll back deploy, run git revert HEAD then redeploy" --kind instruction
recall decide "never push to main without review" --kind feedback --project -
```

Mirror MCP tool: [`record_decision`](#record_decision).

### `recall decisions`

```
recall decisions [--project DIR|all] [--kind K] [--limit N] [--json] [--db PATH]
```

Lists active (non-superseded) decisions for a project (with ancestor scoping), salience-ranked. Each row shows `conf=<0–1>` — the trust score (see [`recall decide`](#recall-decide)).

**Examples:**

```bash
recall decisions                           # for $PWD
recall decisions --project all             # everything
recall decisions --kind instruction        # only runbooks
recall decisions --json --limit 100        # for tooling
```

Mirror MCP tool: [`recall_decisions`](#recall_decisions).

### `recall forget`

Soft-delete a decision. Either by explicit id, or by exact-text match on a pattern-source decision (the auto-extracted ones).

```
recall forget --decision ID | --pattern-text "TEXT" [--db PATH]
```

**Examples:**

```bash
recall forget --decision 23
recall forget --pattern-text "paste long code blocks — cite and summarize"
```

### `recall maintain`

Housekeeping over the decision store, to keep retrieval sharp as it grows. Two passes:

- **Dedup** — merges semantic near-duplicates (cosine ≥ `--dedup-threshold`, default 0.92) *within a project scope*. The most-trustworthy row (confidence, then salience, then recency) wins; each duplicate is superseded, its `use_count` folded in, its phrasing kept as a paraphrase (so the memory stays findable), and the winner's confidence bumped.
- **Decay** — deletes stale, low-salience, **auto-extracted** rows (`source IN (pattern, distilled, surprise)`, never `instruction`) whose effective salience is below `--decay-floor` (0.15) and that haven't been used or created in `--decay-days` (90). User-curated facts are never auto-deleted.

Operates on already-stored embeddings — no embedder or network needed. Runs automatically once every 24h inside [`recall watch`](#recall-watch); this is the manual entry point.

```
recall maintain [--project DIR|-] [--no-dedup] [--no-decay] [--dedup-threshold C] [--decay-floor S] [--decay-days N] [--dry-run] [--json] [--db PATH]
```

**Examples:**

```bash
recall maintain --dry-run           # preview across all projects, change nothing
recall maintain                     # merge dupes + age out stale rows everywhere
recall maintain --project . --no-decay   # only dedup, only this project tree
```

### `recall wiki`

Build and read the **auto-curated entity wiki** — one rolled-up reference card per `@entity` (a person, service, repo, tool, or concept), distilled by Haiku from everywhere that entity has been mentioned. Turns the raw `@`-mention index into "what's durably known about X".

- **Build** (default): for each entity with ≥ `--min-mentions` mentions that has gained mentions since its card was last built, gather its recent excerpts, distil a ≤400-char card, embed it, and store it. Incremental + idempotent. Requires `ANTHROPIC_API_KEY`.
- **`--show NAME`** / **`--list`**: read cards locally (no API key, no network).

```
recall wiki [--show NAME | --list] [--min-mentions N] [--max N] [--dry-run] [--json] [--db PATH]
```

**Examples:**

```bash
ANTHROPIC_API_KEY=sk-ant-... recall wiki      # build/refresh all stale cards
recall wiki --show kong                        # print the @kong card
recall wiki --list                             # all entities that have a card
```

`recall watch` will refresh the wiki once a day automatically **if** you set `RECALL_AUTO_WIKI=1` (and `ANTHROPIC_API_KEY`) — off by default so the daemon never spends API budget unprompted. Mirror MCP tool: [`recall_wiki`](#recall_wiki).

### `recall pin`

Pin a note that always surfaces at the top of the inject block (above Decisions). Pins are session-scoped or project-scoped.

```
recall pin TEXT... [--session SID] [--project DIR|-] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--session` | (none) | Pin only surfaces while this session is active. |
| `--project` | `$PWD` | `-` for global pins. Ignored when `--session` is set. |

**Examples:**

```bash
recall pin "goal for today: ship onnx-embedder PR"
recall pin "remember to roll back the migration" --session "$CLAUDE_SESSION_ID"
```

Mirror MCP tool: [`pin_for_session`](#pin_for_session).

### `recall unpin`

```
recall unpin ID [--db PATH]
```

Soft-clears a pin (sets `cleared_at`). Row stays in the DB for audit but disappears from queries.

### `recall pins`

```
recall pins [--session SID] [--project DIR|all] [--db PATH]
```

List active pins. Default: pins for `$PWD` plus any global / ancestor pins. `--project all` shows everything.

### `recall download-model`

One-time fetch of the ONNX Runtime dylib (~35 MiB, from `github.com/microsoft/onnxruntime/releases`) and the all-MiniLM-L6-v2 sentence-transformer (~22 MiB model + tokenizer + config from `huggingface.co/sentence-transformers/all-MiniLM-L6-v2`). Every artifact is SHA256-verified against pinned hashes; mismatches abort and remove the file.

```
recall download-model [--force]
```

Idempotent. Cached files are re-verified on every run.

### `recall install-model`

Side-load model assets from a local directory (airgap / mirror installs). Same SHA256 verification as `download-model`.

```
recall install-model --from DIR
```

Source dir layout — accepts both:

```
DIR/
  libonnxruntime.dylib
  model_qint8_arm64.onnx
  tokenizer.json
  config.json
```

OR (the same layout as `~/.local/share/recall/`):

```
DIR/
  runtime/libonnxruntime.dylib
  models/all-MiniLM-L6-v2/{model_qint8_arm64.onnx,tokenizer.json,config.json}
```

### `recall embed`

Populate the embedding column on rows missing it. Idempotent — only rows where `embedding IS NULL` are processed.

```
recall embed [--provider onnx|apple] [--scope decisions|turns] [--batch N] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--provider` | onnx if assets present, else apple | Override per call. |
| `--scope` | `decisions` | Decisions get embedded by default. Turns are opt-in (`--scope turns`) since the corpus is bigger. |
| `--batch` | `100` | Max rows to embed per run. |

**Examples:**

```bash
recall embed                                          # decisions, default provider
recall embed --provider apple                         # force Apple NLEmbedding
recall embed --scope turns --batch 500                # embed turn-level for richer recall
```

### `recall paraphrase`

Generate K alternate phrasings per decision via Haiku, embed each locally, store in `decision_paraphrases`. Semantic search takes max cosine across canonical + paraphrase embeddings, broadening recall.

```
recall paraphrase [--limit N] [--per-decision K] [--db PATH]
```

Idempotent — skips decisions that already have ≥K paraphrases.

**Requires `ANTHROPIC_API_KEY`.** Cost: ~$0.001 per decision.

**Examples:**

```bash
recall paraphrase --limit 50 --per-decision 4         # default
recall paraphrase --limit 200 --per-decision 6        # broader coverage
```

### `recall consolidate`

Rewrite session summaries via Haiku 4.5. Picks sessions older than `--days` whose `summary_consolidated_at < ended_at` (i.e. stale or missing). Idempotent.

```
recall consolidate [--days N] [--max N] [--model MODEL] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--days` | `7` | Sessions older than this. |
| `--max` | `25` | Sessions per run. |
| `--model` | `claude-haiku-4-5-20251001` | Override if needed. |

**Requires `ANTHROPIC_API_KEY`.** Cost: ~$0.005 per session.

### `recall distill`

Scan recent turns and extract durable infrastructure facts, runbook procedures, and hard preferences as decisions. Closes the gap between "data is captured in conversation transcripts" and "agent uses captured data" — the agent doesn't always proactively call `record_decision`, so periodic distillation catches what the agent missed.

```
recall distill [--days N] [--batch N] [--max-turns N] [--dry-run] [--json] [--db PATH]
```

| Flag | Default | Notes |
|---|---|---|
| `--days` | `0` (all undistilled turns) | Lookback window. Distilled turns are flagged; re-runs only see new content. |
| `--batch` | `20` | Turns per Haiku call. Larger = fewer calls but higher per-call latency / risk of dropped items. |
| `--max-turns` | `500` | Safety cap per invocation. |
| `--dry-run` | off | Print would-be decisions to stderr; don't write to DB. |
| `--json` | off | Emit aggregate result as JSON instead of human text. |

**What it captures** (per the system prompt sent to Haiku):
- Infrastructure facts (cluster names, namespaces, manifest paths, AWS profiles, IAM patterns, deploy URLs)
- Runbook procedures ("to deploy X, run Y then Z")
- Hard preferences ("we always use Postgres", "never push to main without review")

**What it skips:** task chatter, one-off mid-task decisions, code review back-and-forth.

Idempotent — turns get a `distilled_at` timestamp on first pass, and the dedup in `InsertDecisionIfNew` prevents the same fact landing twice. Decisions get `source='distilled'` so you can filter them with `recall decisions --source distilled` if you want to audit.

**Requires `ANTHROPIC_API_KEY`.** Cost: ~$0.30 per 1000 turns (varies; pure no-op if no key).

Suggested cadence: weekly via cron, or after a high-activity day. Add to your watch daemon plist if you want it automatic.

**Examples:**

```bash
# See what it would extract from the last day, no DB writes
recall distill --days 1 --dry-run

# Process all undistilled turns, default config
recall distill

# Just last week, smaller batches (cheaper / safer for first run)
recall distill --days 7 --batch 10 --max-turns 200
```

### `recall bench longmemeval`

Run the LongMemEval retrieval benchmark and report R@1 / R@5 / R@10. Useful as a regression test after retrieval changes, or to baseline this build vs. published numbers from other memory systems. Mutates a temp SQLite at `$TMPDIR/recall-bench-longmemeval.sqlite` (truncated per question) — your main DB is never touched.

```
recall bench longmemeval <data.json>
  [--limit N] [--skip N]
  [--rrf-k N] [--weights 'lex,sem,temp,kw']
  [--rerank] [--rerank-topk N]
  [--tune] [--dev-size N]
  [--lex-only] [--json] [--db PATH]
```

**Flags:**

| Flag | Default | Notes |
|---|---|---|
| `--limit N` | 0 (all) | Cap questions evaluated. Useful for smoke tests (`--limit 10`). |
| `--skip N` | 0 | Skip the first N questions (used by `--tune` for the held-out split; also useful standalone). |
| `--rrf-k N` | 60 | RRF damping constant. Lower = top-1 hits dominate; higher = ranks 5–15 still contribute. |
| `--weights '…'` | `1,1,1,1` | Comma-separated channel weights for `[lex,sem,temp,keyword]`. Empty = all 1.0. Channels are: FTS5 lexical, cosine over turn embeddings, temporal recency, content-word overlap. |
| `--rerank` | off | After RRF fusion, run a Haiku rerank pass over the top-`--rerank-topk` hits. Requires `ANTHROPIC_API_KEY`; ~$0.001 per question. |
| `--rerank-topk N` | 5 | Number of top fused hits sent to the rerank model. |
| `--tune` | off | Split into a dev set (first `--dev-size` questions) + held-out remainder. Coordinate-descent over `k` and per-channel weights on the dev split, evaluate the winning config on the holdout. Prints both numbers. |
| `--dev-size N` | 50 | Dev-split size when `--tune` is set. |
| `--lex-only` | off | Skip the cosine channel (no embedder). Useful for measuring the FTS-only baseline. |
| `--json` | off | Emit results as JSON; `--tune` JSON includes the winning config. |

**Dataset.** Get the cleaned LongMemEval split from HuggingFace:

```bash
curl -fL -o /tmp/longmemeval_s.json \
  https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json
```

**Examples:**

```bash
# Smoke test (10 q, ~25s)
recall bench longmemeval --limit 10 /tmp/longmemeval_s.json

# Full local run (~20 min, no API key, no $)
recall bench longmemeval /tmp/longmemeval_s.json

# Tune on first 50, evaluate on remaining 450 (~36 min, no $)
recall bench longmemeval --tune --dev-size 50 /tmp/longmemeval_s.json

# Tuned config + Haiku rerank on top of the held-out 450 (~25 min, ~$0.50)
ANTHROPIC_API_KEY=sk-ant-... recall bench longmemeval \
  --skip 50 --rerank --rerank-topk 10 \
  --rrf-k 120 --weights '1.0,0.5,1.0,1.0' \
  /tmp/longmemeval_s.json
```

**Reference numbers** (this repo, ONNX `all-MiniLM-L6-v2-q8`, held-out 450):

| Configuration | R@1 | R@5 | R@10 |
|---|---|---|---|
| Tuned, local-only (no API key) | 87.1% | **98.9%** | 99.1% |
| Tuned + Haiku rerank top-10 | **93.3%** | 98.9% | 99.1% |

### `recall record-tool-event`

Reads a PostToolUse hook payload from stdin (Anthropic's documented hook envelope) and:

- For `Bash` failures (exit ≠ 0 / `is_error` / `interrupted`): records a high-salience surprise decision so the agent learns "this command failed for me here."
- For `Edit` / `Write` / `MultiEdit`: records before/after content hashes in `tool_events`. If a subsequent edit on the same file within 10 minutes has the inverse hash pair, marks an "edit was reverted within 10min" surprise event.

**Wired automatically via the PostToolUse Bash matcher in `~/.claude/settings.json`.** Not invoked by hand.

### `recall watch`

Long-running poll loop. Every interval: ingest from Claude Code / Codex / Cursor / MEMORY.md. Used as a fallback when Claude Code's Stop hook isn't sufficient (e.g. you also use Codex or Cursor). Once every 24 hours it also runs [`recall maintain`](#recall-maintain) (dedup + decay), gated by a marker in `ingest_state` so it fires at most daily regardless of the poll interval.

```
recall watch [--interval 30s] [--db PATH]
```

Install via launchd:

```bash
make watch-install
launchctl load ~/Library/LaunchAgents/ai.fantazm.recall.watch.plist
```

---

## MCP reference

All MCP tools take JSON arguments (per the protocol) and return either a JSON-encoded result or markdown text. The agent inside Claude Code calls these via the host's `tools/call` method.

### `recall_summary`

```
recall_summary(project, days?, budget?)
```

Returns the compact recall block (markdown text). Same content as `recall inject`.

### `recall_search`

```
recall_search(query, mode?, project?, limit?, hyde?)
```

| Param | Notes |
|---|---|
| `query` | Required. Natural language. |
| `mode` | `hybrid` (default), `lexical`, `semantic`. |
| `project` | Filter to one absolute project_dir (with ancestor scoping). |
| `limit` | 1–50. Default 10. |
| `hyde` | Default true when `ANTHROPIC_API_KEY` set. Skips otherwise. |

Returns JSON with `hits[]` (mixed `kind: "turn"` and `kind: "decision"`), `count`, `mode`, `query`, optional `expanded_query` (HyDE), `embedder` name. Each hit has a `channels` array showing which retrievers surfaced it.

### `recall_semantic_search`

```
recall_semantic_search(query, project?, limit?)
```

Cosine-only over decisions. Compares against canonical + paraphrase embeddings; takes max per decision. Always uses HyDE when `ANTHROPIC_API_KEY` set.

### `recall_decisions`

```
recall_decisions(project, kind?, limit?)
```

Active decisions, salience-ranked. Pins are decisions internally and surface here too. Filter by `kind` (`fact`, `preference`, `feedback`, `instruction`).

### `recall_files`

```
recall_files(project, days?, limit?)
```

Recently touched files, ordered by recency × frequency.

### `recall_sessions`

```
recall_sessions(project?, days?, limit?)
```

Recent sessions with summaries.

### `record_decision`

```
record_decision(text, kind?, project?)
```

Record a durable decision. Auto-runs reconsolidation (marks similar existing decisions as `superseded_by` the new one when cosine ≥ 0.85; surfaces 0.65–0.85 matches as "related" candidates in the response without auto-actioning).

The agent in Claude Code calls this proactively when the user states a durable fact / preference / runbook / correction. Use `kind="instruction"` for runbook content so it surfaces in the dedicated Instructions section of every inject.

### `pin_for_session`

```
pin_for_session(text, session_id?, project?)
```

Pin a note. Session-scoped if `session_id` provided; project-scoped otherwise. Returns the pin id so the user can `recall unpin <id>` later.

The agent calls this when the user emphasises something to keep top-of-mind ("remember X", "goal for today", "don't forget Y").

### `recall_wiki`

```
recall_wiki(entity)
```

Fetch the auto-curated reference card for an `@entity` — a rolled-up summary of what's durably known about it across all sessions (built by [`recall wiki`](#recall-wiki)). The agent calls this for "what is X / who is X / tell me about X" when X is a recurring named thing. Returns `found=false` when the entity has no card yet.

---

## Environment variables

| Var | Purpose | Default |
|---|---|---|
| `RECALL_DB` | Override DB path (legacy alias: `AGENT_MEMORY_DB`) | `~/.local/share/recall/db.sqlite` |
| `RECALL_HOME` | Override the assets root — model cache + runtime lib (legacy alias: `AGENT_MEMORY_HOME`) | `~/.local/share/recall` |
| `EMBED_PROVIDER` | Force `onnx` / `apple` for the MCP server's semantic recall | (auto: onnx if assets present, else apple) |
| `OLLAMA_URL` | Ollama base URL when `--provider ollama` is used | `http://localhost:11434` |
| `OLLAMA_MODEL` | Ollama model when `--provider ollama` is used | `nomic-embed-text` |
| `ANTHROPIC_API_KEY` | Enables HyDE, paraphrase generation, consolidate, distill, and the entity wiki. Skipped gracefully when unset. | (none) |
| `RECALL_AUTO_WIKI` | When set (with `ANTHROPIC_API_KEY`), `recall watch` refreshes the entity wiki once a day. Off by default so the daemon never spends API budget unprompted. | (unset) |

---

## Files / paths

| Path | Purpose |
|---|---|
| `~/.local/bin/recall` | The binary |
| `~/.local/share/recall/db.sqlite` | SQLite database (WAL) |
| `~/.local/share/recall/runtime/libonnxruntime.dylib` | ONNX Runtime (downloaded, SHA256-pinned) |
| `~/.local/share/recall/models/all-MiniLM-L6-v2/` | Sentence-transformer + tokenizer (downloaded, SHA256-pinned) |
| `~/.local/state/recall/ingest.log` | Stop-hook ingest output |
| `~/.local/state/recall/hook.err` | PostToolUse hook stderr |
| `~/.claude/settings.json` | Claude Code hooks config |
| `~/.claude.json` | Claude Code MCP server registration (Claude Code 2.1+) |
| `~/.claude/CLAUDE.md` | Global agent instructions (mentions recall_* tools) |
| `~/Library/LaunchAgents/ai.fantazm.recall.watch.plist` | Watch daemon (after `make watch-install`) |
