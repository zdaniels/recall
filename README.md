<p align="center">
  <img src="assets/logo.png" alt="recall" width="220" />
</p>

<h1 align="center">Fathom Recall</h1>

<p align="center">
  <a href="https://github.com/zdaniels/recall/releases/latest"><img src="https://img.shields.io/github/v/release/zdaniels/recall?color=cobalt" alt="release"></a>
  <a href="https://github.com/zdaniels/recall/actions/workflows/release.yml"><img src="https://github.com/zdaniels/recall/actions/workflows/release.yml/badge.svg" alt="build"></a>
  <a href="https://github.com/zdaniels/recall/stargazers"><img src="https://img.shields.io/github/stars/zdaniels/recall?style=social" alt="stars"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/zdaniels/recall" alt="license"></a>
</p>

<p align="center"><strong>One shared memory across all of your coding agents.</strong><br/>
Claude Code · Codex · Cursor · Hermes · Cline · Aider · Continue · Goose · Zed · Roo Code · Kilo Code.<br/>
98.9% R@5 on LongMemEval. No API key. Single Go binary.</p>

<p align="center">
A decision you make in Claude Code surfaces in Cursor. A path you found
in Codex shows up in Aider. recall reads every agent's local session
storage into <strong>one SQLite database</strong> and injects the
relevant slice back into whichever tool you open next — so your context
follows you across all 11 instead of being trapped in the one you
happened to be using.
</p>

<p align="center">
  <a href="#install"><strong>Install →</strong></a> ·
  <a href="#benchmark">Benchmark</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#faq">FAQ</a>
</p>

---

## Install

macOS (arm64), Linux (x64 + arm64), and Windows (x64). **No Go required** — the installer drops a prebuilt, signed binary; Go is only needed if you [build from source](#build-from-source-requires-go).

### Prebuilt binary (no Go required)

**curl (macOS / Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.sh | sh
recall download-model      # one-time, ~57 MiB, SHA256-verified
recall doctor              # sanity check
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.ps1 | iex
recall download-model      # one-time, ~57 MiB, SHA256-verified
recall doctor              # sanity check
```

The installer downloads the latest signed release archive, verifies its SHA256, drops the binary at `~/.local/bin/recall` (`%USERPROFILE%\.local\bin\recall.exe` on Windows), and prints next steps. No Go toolchain, no build step. Set `RECALL_FULL=1` (`$env:RECALL_FULL=1`) for the larger archive that bundles the ONNX runtime + model (for airgapped or restricted-egress machines). The installer sources are [`dist/install.sh`](./dist/install.sh) / [`dist/install.ps1`](./dist/install.ps1).

Prefer not to pipe to a shell? Download the per-platform tarball from [Releases](https://github.com/zdaniels/recall/releases) and verify its `.sha256` — also no Go needed — or [build from source](#build-from-source-requires-go) if you'd rather compile it yourself.

### Wire it into your tools

Once the binary is installed, wire every supported tool found on this machine — Claude Code MCP + hooks, Codex MCP + AGENTS.md, Cursor `mcp.json`, Hermes `~/.hermes/config.yaml` — and backfill your existing session history. For v0.1, the wiring lives in a setup script in this repo:

```bash
git clone https://github.com/zdaniels/recall.git
cd recall && ./dist/setup.sh        # macOS / Linux
#            pwsh -File dist/setup.ps1   # Windows (PowerShell 7+)
```

`dist/setup.sh` (and the `dist/setup.ps1` Windows port) is idempotent. Re-running merges new config without touching anything the script didn't put there. A `recall setup` subcommand that does the same thing without needing the repo clone is planned for v0.1.0.

One manual step the script can't automate: Cursor's user-rules tier is server-side, so paste `dist/cursor-user-rule.md` into Cursor → Settings → Rules → User Rules. Skip if you don't use Cursor. Similarly for Hermes, add `dist/hermes-rule.md` to your Hermes system prompt or a skill (the MCP server itself is wired automatically), then `/reload-mcp`.

After setup, your next Claude Code / Codex / Cursor session opens with a `<recall>` block injected automatically: recent sessions, files you've touched, decisions you've made, all scoped to the current project.

### Build from source (requires Go)

Only needed if you're hacking on recall or want to compile it yourself — **regular installs don't need this.** Requires Go 1.26+, make, and a working C toolchain — the ONNX Runtime bridge is CGO on every platform (gcc/clang on Linux, Xcode CLT on macOS, mingw-w64 on Windows). On macOS the Apple NaturalLanguage framework is also linked for the optional on-device fallback embedder.

```bash
git clone https://github.com/zdaniels/recall.git
cd recall
make setup
```

The `make setup` target builds the binary then runs the same `dist/setup.sh` the installer runs after download.

## Benchmark

Measured on the [LongMemEval](https://github.com/xiaowu0162/LongMemEval) cleaned dataset (500 questions; 50 dev / 450 held-out split). Reproducible from this repo with one command.

**No API key required — fully local.** ONNX `all-MiniLM-L6-v2` quantized embedder, ~57 MiB of local assets, zero network calls at query time.

| Metric | recall (local only) | recall (+ Haiku rerank top-10) |
|---|---|---|
| **R@5** (held-out 450) | **98.9%** (445/450) | 98.9% (445/450) |
| R@10 (held-out 450) | 99.1% (446/450) | 99.1% (446/450) |
| R@1  (held-out 450) | 87.1% (392/450) | **93.3%** (420/450) |

The **98.9% R@5 is the no-LLM result** — what you get from `make setup` with no extra config. To our knowledge it is at or above the highest published held-out R@5 for any local-first memory system on this dataset; most published numbers from comparable systems sit between 96% and 98.4% on the same protocol. Adding a Haiku rerank pass over the top-10 fused candidates lifts R@1 by **+6.2pp** without changing R@5 — useful when the consuming agent reads only the top-1 hit, otherwise unnecessary.

### Reproduce locally

No API key, ~36 minutes, ~$0:

```bash
recall bench longmemeval --tune --dev-size 50 path/to/longmemeval_s_cleaned.json
```

`--tune` does coordinate descent over the RRF damping constant `k` and per-channel weights on the dev split, then evaluates the winning config on the held-out remainder.

To also exercise the rerank path (adds ~$0.50 in Haiku calls):

```bash
ANTHROPIC_API_KEY=sk-ant-... recall bench longmemeval \
    --skip 50 --rerank --rerank-topk 10 \
    --rrf-k 120 --weights '1.0,0.5,1.0,1.0' \
    path/to/longmemeval_s_cleaned.json
```

Tuned config from this run: `k=120, weights=[lex=1.0, sem=0.5, temp=1.0, kw=1.0]`. Channels are FTS5 lexical, cosine over turn embeddings, temporal recency, and content-word overlap — fused via Reciprocal Rank Fusion. Full methodology in [BENCHMARKS.md](./BENCHMARKS.md); production retrieval lives in `internal/recall/search.go` and the benchmark harness in `internal/bench/longmemeval.go`.

### How recall compares

Most memory-system numbers people quote are **end-to-end QA accuracy** (retrieval + an LLM reader answers the question correctly), not retrieval-only metrics. recall's 98.9% is a **retrieval** number — "the answer is in the top-5 chunks we surfaced." It's a different (and more decomposable) measurement.

| System | R@5 (LongMemEval held-out) | Local-only? | API key? | Install |
|---|---|---|---|---|
| **recall** | **98.9%** | ✅ | ❌ | single Go binary |

> Apples-to-apples rows for Mem0 / Letta / Zep / Supermemory are deliberately omitted: their published numbers are mostly end-to-end QA accuracy with a specific reader model, not raw retrieval R@5. A head-to-head retrieval-only sweep is on the roadmap; until then, treat "98.9% R@5, local, no key" as the load-bearing claim.

## How it works

- **Reads** session storage from every supported tool — eleven sources in v0.1:
  - **Claude Code** — `~/.claude/projects/*.jsonl`
  - **Codex** — `~/.codex/state_5.sqlite` (`threads` table)
  - **Cursor** — `~/Library/.../Cursor/.../state.vscdb` (`composer.composerHeaders`)
  - **Hermes Agent** (NousResearch) — `~/.hermes/state.db` (Linux/macOS/WSL2; no native Windows app, so on Windows it's picked up by the Linux build running inside WSL2). **Bidirectional:** recall also registers itself as an MCP server in `~/.hermes/config.yaml`, so Hermes can query the shared store (`recall_search`, `record_decision`, …) — not just feed it.
  - **Cline** (VS Code) — `~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks/<taskId>/`
  - **Aider** — `<project>/.aider.chat.history.md`
  - **Continue** (VS Code / JetBrains) — `~/.continue/sessions/<sessionId>.json`
  - **Goose** (Block) — `~/Library/Application Support/Block/goose/sessions/sessions.db`
  - **Zed AI panel** — `~/Library/Application Support/Zed/threads/threads.db` (zstd-compressed JSON blobs)
  - **Roo Code** (VS Code) — `~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks/<taskId>/`
  - **Kilo Code** (VS Code, Roo successor) — `~/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code/tasks/<taskId>/`
  
  Everything lands in a normalized SQLite database at `~/.local/share/recall/db.sqlite`.
- **Detects decisions** in your user turns via regex patterns ("don't X", "let's go with Y", "from now on Z", ...) and stores them as durable, project-scoped memory.
- **Injects** a query-aware `<recall>` block at SessionStart (Claude Code hook): recent sessions, recently-touched files, active decisions scoped to the current project (with ancestor-prefix scoping, so `/Users/z` decisions surface in `/Users/z/Documents/sub-project`).
- **Exposes recall** as an MCP stdio server — 9 tools: `recall_search`, `recall_semantic_search`, `recall_summary`, `recall_decisions`, `recall_files`, `recall_sessions`, `record_decision`, `pin_for_session`, `recall_wiki`. Implemented in stdlib JSON-RPC — no third-party MCP code path on the agent-facing wire.
- **Polls** every non-Claude-Code source every 30s via `recall watch` (Claude Code uses its Stop hook so it doesn't need polling), and runs a daily maintenance pass that merges duplicate decisions and ages out stale ones.

See [DESIGN.md](./DESIGN.md) for the three-tier memory model, schema, reliability invariants, and roadmap. See [USAGE.md](./USAGE.md) for the full command + flag reference.

## Why this exists

Three things go wrong with most agent memory today:

1. **It's not local.** Mem0, Zep, Supermemory all want your conversations in their cloud (or running their hosted service). Code review transcripts, internal architecture discussions, half-written secrets — none of that should leave the machine.
2. **It's siloed per tool.** Decisions you made in Claude Code don't surface in Cursor. Cursor's sessions don't surface in Codex. Eleven tools, eleven blind spots. recall is the opposite by design: one shared store every agent reads from and writes to, so context earned in any tool is available in all of them.
3. **It requires an API key.** Most memory systems use a hosted embedder, which means cost-per-token, network latency, and another vendor in the privacy boundary.

recall is what we shipped after running into these problems: one SQLite DB, one MCP server, no network calls at query time, and a benchmark that says it actually works.

## FAQ

**How is this different from Mem0 / Letta / Zep / Supermemory?**
recall is local-only (no API key at query time), reads from your coding agents' on-disk session storage directly (no manual `mem.add(...)` calls), and ships as a single Go binary. The closest comparison architecturally is QMD (Shopify's internal local-first memory system) — same instinct, public + benchmarked.

**Why SQLite, not a vector database?**
Hybrid retrieval (FTS5 + cosine + recency + content-word overlap, fused via RRF) outperforms cosine-only on this dataset. SQLite ships FTS5 in stdlib, so the binary stays one file with no Postgres/pgvector/Qdrant operational footprint. The cosine scan is pure Go — no C extension, no `sqlite-vec` build dance.

**What about privacy? You're reading my sessions.**
recall reads files that are already on your machine (Claude Code, Codex, Cursor each persist sessions locally). It does not upload anything. The only network calls are: the one-time ONNX model download (SHA256-pinned, from huggingface.co + github.com/microsoft/onnxruntime) and, only if you explicitly run `recall paraphrase` / `recall distill` / `recall consolidate`, calls to `api.anthropic.com` using **your** API key. The default `make setup` flow makes zero LLM calls.

**Why should I trust this with my sessions?**
The whole thing is one Go binary you can audit — about 15k lines, MIT-licensed, no third-party MCP code path on the agent-facing wire. See [SBOM.md](./SBOM.md) for the full dependency tree and network destinations. If you'd rather not trust us, fork it.

## Commands

Short list — full reference (every flag, every example, common workflows, MCP tool details) lives in [USAGE.md](./USAGE.md).

| Command | What it does |
|---|---|
| `recall search QUERY` | Hybrid search (FTS5 + semantic) — default tool for "find me anything matching X" |
| `recall ingest` | Read source agents' session storage into the DB (idempotent; auto on Stop hook) |
| `recall inject` | Render the `<recall>` block (auto on SessionStart hook) |
| `recall decide TEXT` | Record a fact / preference / feedback / instruction |
| `recall pin TEXT` | Pin a note for the rest of the session or project |
| `recall pins`, `recall unpin ID` | List / clear pins |
| `recall decisions` | List active decisions for a project |
| `recall forget --decision ID` | Soft-delete a decision |
| `recall maintain` | Merge duplicate decisions + age out stale ones (runs daily inside `recall watch`) |
| `recall wiki` | Build / show auto-curated per-`@entity` summary cards (build needs `ANTHROPIC_API_KEY`) |
| `recall stats` | DB counts + date range |
| `recall doctor` | Self-check: binary, DB, ONNX assets, and per-tool wiring |
| `recall download-model` | One-time fetch of ONNX Runtime + sentence-transformer (SHA256-pinned) |
| `recall install-model --from DIR` | Airgap / mirror install with hash verification |
| `recall embed` | Populate embeddings on stored rows |
| `recall paraphrase` | Generate alt-phrasings per decision (requires `ANTHROPIC_API_KEY`) |
| `recall consolidate` | Rewrite old session summaries via Haiku (requires `ANTHROPIC_API_KEY`) |
| `recall distill` | Extract durable facts / runbooks from recent turns into decisions (requires `ANTHROPIC_API_KEY`) |
| `recall bench longmemeval <data>` | Run the LongMemEval retrieval benchmark; reports R@1/R@5/R@10. `--tune` does coordinate descent over RRF k + per-channel weights on a dev split |
| `recall watch` | Poll Codex / Cursor / Hermes on a timer; runs the daily maintenance pass |
| `recall mcp` | Stdio MCP server (spawned by Claude Code) |
| `recall record-tool-event` | Capture PostToolUse hook events (auto-wired) |

## Status

**Landed:**
- **Substrate.** SQLite + WAL, FTS5 (porter stemming), schema v9. Stdlib-only MCP stdio server (no third-party MCP code path). Single static Go binary; macOS (arm64), Linux (x64 + arm64), Windows (x64).
- **11-source ingest.** Read-only adapters for Claude Code, Codex, Cursor, Hermes, Cline, Aider, Continue, Goose, Zed, Roo Code, Kilo Code — normalized into one DB. Claude Code via Stop / SessionStart / PostToolUse / PreCompact hooks; the rest via the 30s watch daemon.
- **MCP wired in four tools.** Claude Code, Codex, Cursor, and Hermes register the `recall` MCP server (9 tools); agent rules nudge proactive `recall_search` use. Hermes is bidirectional — recall reads its sessions *and* serves it.
- **Hybrid retrieval.** FTS5 lexical + cosine (turn/decision embeddings) + temporal recency + content-word overlap, fused via Reciprocal Rank Fusion with tunable per-channel weights. **R@5 = 98.9% / R@10 = 99.1%** local-only on LongMemEval held-out; +Haiku rerank lifts R@1 from 87% → 93% without changing R@5.
- **Decisions + salience + trust.** Regex decision extraction, salience math (time decay + use_count), edit-revert + surprise capture, co-occurrence edges, MEMORY.md sync. Confidence/trust scoring (schema v8): re-asserted facts get reinforced, contradicted ones decay; folded into ranking and shown as `✓` in the injected block. Entity scoping via `@<name>` mentions (schema v6).
- **Maintenance (`recall maintain`).** Daily dedup (merge near-duplicate decisions) + decay (age out stale, low-salience auto-extracted rows), folded into `recall watch`.
- **Entity wiki (`recall wiki`, schema v9).** Haiku-distilled per-`@entity` summary cards, refreshed incrementally, surfaced via the `recall_wiki` MCP tool. Opt-in.
- **Optional Haiku passes.** `recall paraphrase` (alt-phrasings, schema v5), `recall distill` (turns → durable decisions, schema v7), `recall consolidate` (tighten old session summaries). All opt-in; the default install makes zero LLM calls.
- **One-shot install.** `dist/setup.sh` / `setup.ps1` build, download the SHA256-pinned model, and wire every detected tool's MCP + hooks + agent rules (marker-bracketed merges preserve your content).

**Roadmap:**
- Spreading-activation ranking that walks the edges graph at query time (today it only seeds the injected block).
- Summarise-into-parent forgetting curve (the decay pass currently deletes rather than summarising).
- Cross-machine sync via a git-friendly markdown export.

## License

MIT. Fork freely.

## A small lab in the open

recall is part of [Fathom](https://github.com/zdaniels/fathom) — open-source, local-first, security-first AI tooling.
