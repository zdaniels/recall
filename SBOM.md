# SBOM — recall

Inventory of every external component the `recall` binary depends on, plus the runtime artifacts it downloads. Reflects `main` as of v0.1.0.

This file exists for security review. It mirrors `go.mod` plus the artifacts pulled by `recall download-model` / `recall install-model`.

## Static dependencies (linked into the `recall` binary)

These ship with every build. License obligations apply to the binary you distribute.

| Module | Version | Source | License | Purpose |
|---|---|---|---|---|
| `modernc.org/sqlite` | v1.50.0 | modernc.org (Jan Mercl) | BSD-3-Clause | Pure-Go SQLite — primary backing store at `~/.local/share/recall/db.sqlite` |
| `modernc.org/libc` | v1.72.0 | modernc.org | BSD-3-Clause | Required by modernc.org/sqlite |
| `modernc.org/{memory, mathutil, fileutil, sortutil, strutil, opt, token}` | various | modernc.org | BSD-3-Clause | Utility deps of modernc.org/sqlite |
| `modernc.org/{cc/v4, ccgo/v4, gc/v2, gc/v3, goabi0}` | various | modernc.org | BSD-3-Clause | Build-time deps of modernc.org/libc; pruned from runtime via Go's dead-code elimination |
| `golang.org/x/sys` | v0.42.0 | golang.org | BSD-3-Clause | Syscall bindings (transitive via modernc.org/libc) |
| `golang.org/x/text` | v0.36.0 | golang.org | BSD-3-Clause | Unicode NFD normalization in our BERT tokenizer (`internal/embed/tokenizer.go`) |
| `github.com/google/uuid` | v1.6.0 | github.com/google/uuid (Google) | BSD-3-Clause | UUID parsing (transitive) |
| `github.com/mattn/go-isatty` | v0.0.20 | github.com/mattn (Yasuhiro Matsumoto) | MIT | TTY detection (transitive) |
| `github.com/dustin/go-humanize` | v1.0.1 | github.com/dustin (Dustin Sallings) | MIT | Number formatting (transitive) |
| `github.com/ncruces/go-strftime` | v1.0.0 | github.com/ncruces (Nuno Cruces) | MIT | Date formatting used by modernc.org/sqlite |
| `github.com/remyoudompheng/bigfft` | (pinned) | github.com/remyoudompheng | BSD-3-Clause | Big-integer FFT used by modernc.org/sqlite |
| `github.com/klauspost/compress` | v1.18.6 | github.com/klauspost (Klaus Post) | BSD-3-Clause | zstd decompression for the Zed adapter (`internal/adapter/zed`) — Zed stores AI-thread blobs zstd-compressed |
| **standard library** | go1.26.x | golang.org | BSD-3-Clause | All MCP wire I/O, HTTP, JSON, sqlite SQL, hooks, regexp, etc — implemented against `encoding/json`, `bufio`, `database/sql`, `net/http`, `os/exec`, `crypto/sha256` only |

**Verification**: every transitive module is checksum-locked in `go.sum`. `go mod verify` confirms hashes against the local module cache.

### On the four small individual-maintainer transitive deps

`mattn/go-isatty`, `dustin/go-humanize`, `ncruces/go-strftime`, and `remyoudompheng/bigfft` are pulled in by `modernc.org/sqlite`. We don't import any of them directly. The case for accepting them rather than forking SQLite to remove them:

| Lib | What it does for sqlite | Why it's defensible |
|---|---|---|
| `mattn/go-isatty` (~200 LOC) | TTY detection in modernc.org/libc's stdio shim | Yasuhiro Matsumoto is a top-tier Go contributor; library has been stable since 2014; deployed in Docker, kubectl, Hugo, Caddy, and ~30k+ public Go projects. No I/O, just a `syscall.SyscallN` to `isatty(3)`. |
| `dustin/go-humanize` (~600 LOC) | Number / byte / time formatting in error messages | Dustin Sallings (formerly Couchbase principal). Stable since 2014. Used by Etcd, Hugo, Caddy. Pure arithmetic + string formatting — no I/O, no syscalls. |
| `ncruces/go-strftime` (~250 LOC) | C `strftime()` semantics for the SQL `strftime()` function | Smaller maintainer but the library is trivially auditable: ~250 lines of pure date math, deterministic by design, no I/O. We could vendor it in an afternoon if pushed. |
| `remyoudompheng/bigfft` (~700 LOC) | `O(n log n)` integer multiplication for SQL operations on very large numbers | Specialized number-theoretic transform implementing FFT-based multiplication. Pure math; no I/O. Replacing it ourselves is high-risk (subtle bugs in math libs that don't surface in unit tests are real). Only invoked for huge-number arithmetic, which we don't trigger in normal recall workloads. |

**Risk assessment.** All four are pure compute — no network, no filesystem, no syscalls beyond standard libc/runtime calls. None is on recall's critical recall path; none is reachable from external input we accept.

**Why not replace them.** Removing them requires forking `modernc.org/sqlite` itself (a much larger codebase, also by an individual maintainer). Accepting modernc.org/sqlite while rejecting the four helpers it pulls would be inconsistent — Mercl's SQLite port is industry-standard pure-Go SQLite used by Caddy, GoLand, FreshRSS, Pocketbase, and many other security-conscious shops. If your CISO accepts that, the four helpers are part of the package.

**If pushed.** We can vendor `isatty` / `humanize` / `strftime` (vendor-and-fork modernc.org/sqlite to use the vendored copies). Cost: ~2 hours one-time + minor friction on every modernc.org/sqlite version bump. We'd recommend leaving `bigfft` alone — replacing FFT-based big-integer multiplication ourselves trades a known-working library for code we'd own that has a real chance of subtle bugs.

**Notable absences (deliberate)**:
- No third-party MCP library — wire protocol is implemented against stdlib `encoding/json` + `bufio` (~250 LOC, `internal/mcp/server.go`)
- No third-party tokenizer — BERT WordPiece is hand-rolled (~210 LOC, `internal/embed/tokenizer.go`)
- No third-party ONNX bindings — we vendor only Microsoft's official MIT-licensed C header and call ~25 functions through our own ~250-LOC CGO wrapper (`internal/onnx/bridge.{h,c}` + `onnx.go` + `session.go`)
- No telemetry, analytics, error reporting, or update-check libraries

## Runtime CGO bridges (compiled into the binary)

| Bridge | What it links | Where | Purpose |
|---|---|---|---|
| `internal/embed/apple_darwin.go` | `Foundation`, `NaturalLanguage` (OS-bundled) | macOS only | Fallback embedder using Apple's `NLEmbedding` (on-device, ships with the OS) |
| `internal/onnx/bridge.{h,c}` + Go wrappers | dynamically `dlopen`s `libonnxruntime.dylib` (downloaded, see below) | macOS / Linux | Our own minimal CGO bridge to ONNX Runtime — vendored MIT header (`onnxruntime_c_api.h` + 5 transitive headers) + ~250 LOC of C wrapper. No third-party Go bindings. |

## Downloaded artifacts (NOT in the binary; pinned by SHA256)

These are fetched once via `recall download-model` (or side-loaded via `recall install-model --from DIR`) and cached at `~/.local/share/recall/`. Every download is SHA256-verified against the constants in `internal/embed/assets.go`; mismatches cause hard failures and the file is removed.

| Artifact | Size | Origin | License | SHA256 (pinned) |
|---|---|---|---|---|
| `libonnxruntime.dylib` (extracted from `onnxruntime-osx-arm64-1.25.1.tgz`) | 35 MiB | `github.com/microsoft/onnxruntime/releases/v1.25.1` | MIT (Microsoft) | `c009754f5c160014ee302b28f44b888ad95c841d06e343fb953f5894beee81a4` |
| Tarball SHA256 (verified pre-extraction) | 22 MiB | same | MIT | `18987ec3187b5f29ba798109750f6135060560ad4e0a52678fcc753ee8fb3091` |
| `model_qint8_arm64.onnx` | 22 MiB | `huggingface.co/sentence-transformers/all-MiniLM-L6-v2` | Apache-2.0 (Nils Reimers / sentence-transformers) | `4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474` |
| `tokenizer.json` | 455 KiB | same | Apache-2.0 | `be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037` |
| `config.json` | 612 B | same | Apache-2.0 | `953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41` |

**Total disk cost** at `~/.local/share/recall/`: ~57 MiB.

## Network destinations

`recall` makes outbound network calls **only** in these scenarios:

| Command | Destination | Purpose | Verifiable? |
|---|---|---|---|
| `recall download-model` | `api.github.com` (HTTPS) | Latest ONNX Runtime version lookup | Yes — Microsoft official |
| `recall download-model` | `github.com/microsoft/onnxruntime/releases/...` (302 → S3) | ONNX Runtime tarball | Yes — SHA256 verified before extraction |
| `recall download-model` | `huggingface.co/sentence-transformers/...` (302 → xet-bridge S3) | Model + tokenizer + config | Yes — SHA256 verified per file |
| `recall consolidate` | `api.anthropic.com/v1/messages` | Optional Haiku summarisation, opt-in via `ANTHROPIC_API_KEY` | Same destination Claude Code already talks to |
| `recall_search` (semantic / hybrid mode with HyDE) | `api.anthropic.com/v1/messages` | Optional query expansion via Haiku (auto-on when `ANTHROPIC_API_KEY` set; cached in-process for 1h) | Same destination |
| `recall paraphrase` (and auto-trigger from `record_decision` when key present) | `api.anthropic.com/v1/messages` | Optional alternate-phrasing generation via Haiku, embedded locally | Same destination |
| `recall distill` | `api.anthropic.com/v1/messages` | Optional turn-content → decision distillation via Haiku (opt-in; ~$0.30 per 1000 turns). Falls back to a silent no-op when `ANTHROPIC_API_KEY` is unset. | Same destination |
| `recall_search` (with `rerank=true`) and `recall bench longmemeval --rerank` | `api.anthropic.com/v1/messages` | Optional Haiku rerank pass over the top-K fused candidates (~$0.001 per query). Off by default; gracefully no-ops without an API key. | Same destination |

**Zero network calls** during recall queries (`recall_search`, `recall_semantic_search`, etc), session-start injection, or any normal day-to-day usage. The binary makes no telemetry calls, no update checks, no crash reporting.

## Airgap / mirror install

For environments that block egress to huggingface.co or github.com:

```bash
# On a machine with internet access:
recall download-model
cp -r ~/.local/share/recall/{runtime,models}/ /shared-internal-mirror/recall-assets/

# On the airgapped target machine:
recall install-model --from /shared-internal-mirror/recall-assets/
```

`install-model` re-verifies each file against the same pinned SHA256s before promoting it into place. The pinned hashes are version-controlled in source — your CI pipeline can lock down a specific commit.

## Update process

The pinned hashes update only via deliberate edits to `internal/embed/assets.go`. Procedure:

1. Bump `OrtVersion` if updating the runtime, or change the model branch / repo if updating the model.
2. Run `recall download-model --force` on a known-good box.
3. `shasum -a 256 ~/.local/share/recall/{runtime/libonnxruntime.dylib,models/all-MiniLM-L6-v2/*}`
4. Replace the constants. Rebuild. Run the existing tokenizer + semantic-search tests.
5. Commit. The diff is auditable: every byte that changes downstream traces to a SHA256 change in source.

## License obligations summary

- All Go deps are BSD/MIT — attribution-only, permissive.
- `Apache-2.0` (model weights, tokenizer): attribution + NOTICE preservation when redistributing the model files. We do not embed the weights in the binary; they live in user-space assets the user fetches.

## Vendored MIT C headers (in-repo)

| Path | Source | License |
|---|---|---|
| `internal/onnx/onnxruntime_c_api.h` (and 5 transitive `onnxruntime_*_c_api.h` siblings) | github.com/microsoft/onnxruntime release v1.25.1 | MIT (Microsoft) — `internal/onnx/ONNXRUNTIME_LICENSE` preserved verbatim |

These ship in source so the CGO wrapper compiles without an external SDK install. They're documentation of the ABI we call against — no logic, just declarations.
