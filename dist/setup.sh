#!/usr/bin/env bash
#
# recall bootstrap — one-shot, idempotent.
#
# Wires every tool found on this machine:
#   - Claude Code  → MCP register + SessionStart/Stop/PostToolUse/PreCompact
#                    hooks + ~/.claude/CLAUDE.md recall rule block
#   - Codex CLI    → MCP register + ~/.codex/AGENTS.md drop-in
#   - Codex.app    → symlink the bundled CLI onto PATH so the above works
#   - Cursor       → ~/.cursor/mcp.json (User Rule paste is the one manual step)
#   - Hermes       → ~/.hermes/config.yaml mcp_servers entry (rule paste is manual)
#   - watch daemon → launchd plist (macOS) or systemd user unit (Linux)
#   - ONNX model   → recall download-model (~57 MiB, SHA256-verified)
#
# Re-run safe — every step checks state before acting. Skips cleanly if a tool
# isn't installed.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PREFIX="${PREFIX:-$HOME/.local}"
RECALL="$PREFIX/bin/recall"

step() { printf '\n\033[1m▸ %s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
warn() { printf '  \033[33m! %s\033[0m\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

# merge_marker_block <target-file> <source-content-file> <begin-marker> <end-marker>
#
# Drop the contents of <source-content-file> into <target-file>, bracketed by
# <begin-marker>/<end-marker>. Re-runs replace only the content between the
# markers; everything outside is preserved.
#
#   - target absent           → write the bracketed block fresh.
#   - target has markers      → replace block; surrounding content untouched.
#   - target without markers, content matches source → migrate to markers.
#   - target without markers, custom content → leave alone, print warning.
merge_marker_block() {
  local target="$1" source="$2" begin="$3" end="$4"
  local block_file
  block_file=$(mktemp)
  {
    printf '%s\n' "$begin"
    cat "$source"
    printf '%s\n' "$end"
  } > "$block_file"

  mkdir -p "$(dirname "$target")"
  if [[ ! -f "$target" ]]; then
    cp "$block_file" "$target"
    info "wrote $target"
  elif grep -qF "$begin" "$target"; then
    local tmp
    tmp=$(mktemp)
    awk -v block_file="$block_file" -v begin="$begin" -v end="$end" '
      BEGIN {
        in_block = 0
        block = ""
        while ((getline line < block_file) > 0) block = block line "\n"
        close(block_file)
        sub(/\n$/, "", block)
      }
      $0 == begin { print block; in_block = 1; next }
      $0 == end   { in_block = 0; next }
      !in_block   { print }
    ' "$target" > "$tmp"
    if cmp -s "$target" "$tmp"; then
      info "$target already up to date"
      rm -f "$tmp"
    else
      mv "$tmp" "$target"
      info "recall block updated in $target (other content preserved)"
    fi
  elif cmp -s "$source" "$target"; then
    cp "$block_file" "$target"
    info "$target migrated to use recall markers (content unchanged)"
  else
    warn "$target exists without recall markers — leaving it alone."
    warn "Append the block from $source to $target,"
    warn "or wrap your existing block with these markers so re-runs update it:"
    info "  $begin"
    info "  $end"
  fi
  rm -f "$block_file"
}

# ─── 1. Build + install the binary ───────────────────────────────────────
step "1. Build + install recall"
if [[ ! -x "$RECALL" ]]; then
  make install >/dev/null
  info "installed → $RECALL"
else
  info "already installed → $RECALL ($("$RECALL" version 2>&1))"
fi

# Make sure ~/.local/bin is on PATH for this shell run
export PATH="$PREFIX/bin:$PATH"

# ─── 2. Download ONNX model + assets ─────────────────────────────────────
step "2. Download ONNX model (idempotent, SHA256-verified)"
"$RECALL" download-model

# ─── 3. Claude Code wiring ───────────────────────────────────────────────
step "3. Claude Code"
if have claude; then
  if ! claude mcp list 2>/dev/null | grep -q '^recall'; then
    claude mcp add --scope user recall "$RECALL" mcp >/dev/null
    info "MCP server registered → ~/.claude.json"
  else
    info "MCP server already registered"
  fi

  # Merge our hooks into ~/.claude/settings.json without clobbering anything.
  # For each event (SessionStart / PostToolUse / Stop), append our hook entry
  # to the existing array iff no entry with the same command string is already
  # there. Other top-level keys and unrelated hooks survive untouched.
  SETTINGS="$HOME/.claude/settings.json"
  mkdir -p "$HOME/.claude"

  HOOK_INJECT='recall inject --prompt-stdin --budget 1500 2>/dev/null'
  HOOK_TOOL='recall record-tool-event 2>>~/.local/state/recall/hook.err'
  HOOK_STOP='mkdir -p ~/.local/state/recall && recall ingest >> ~/.local/state/recall/ingest.log 2>&1'
  # PreCompact fires before Claude Code compresses session context — without
  # this, turns added between the last Stop and the compaction can be lost
  # from on-disk capture (the JSONL transcript may be summarized in place).
  HOOK_PRECOMPACT='mkdir -p ~/.local/state/recall && recall ingest >> ~/.local/state/recall/ingest.log 2>&1'

  if ! have jq; then
    warn "jq not on PATH — skipping hook merge. Add these into $SETTINGS .hooks by hand:"
    cat <<JSON
  "SessionStart": [{ "hooks": [{ "type": "command", "command": "$HOOK_INJECT" }] }],
  "PostToolUse":  [{ "matcher": "Bash", "hooks": [{ "type": "command", "command": "$HOOK_TOOL" }] }],
  "Stop":         [{ "hooks": [{ "type": "command", "command": "$HOOK_STOP" }] }],
  "PreCompact":   [{ "hooks": [{ "type": "command", "command": "$HOOK_PRECOMPACT" }] }]
JSON
  else
    [[ -f "$SETTINGS" ]] || echo '{}' > "$SETTINGS"
    tmp=$(mktemp)
    jq \
      --arg inject     "$HOOK_INJECT" \
      --arg tool       "$HOOK_TOOL" \
      --arg stop       "$HOOK_STOP" \
      --arg precompact "$HOOK_PRECOMPACT" '
      def has_cmd(cmd): any(.. | objects | select(.command? == cmd); true);
      def add_hook(ev; entry; cmd):
        .hooks[ev] = (
          (.hooks[ev] // [])
          | if has_cmd(cmd) then . else . + [entry] end
        );
      (.hooks //= {})
      | add_hook("SessionStart";
                 {hooks:[{type:"command", command:$inject}]};
                 $inject)
      | add_hook("PostToolUse";
                 {matcher:"Bash", hooks:[{type:"command", command:$tool}]};
                 $tool)
      | add_hook("Stop";
                 {hooks:[{type:"command", command:$stop}]};
                 $stop)
      | add_hook("PreCompact";
                 {hooks:[{type:"command", command:$precompact}]};
                 $precompact)
    ' "$SETTINGS" > "$tmp"
    if cmp -s "$SETTINGS" "$tmp"; then
      info "hooks already present in $SETTINGS — no changes"
      rm -f "$tmp"
    else
      mv "$tmp" "$SETTINGS"
      info "hooks merged into $SETTINGS (existing hooks preserved)"
    fi
  fi

  # Drop our agent rule into ~/.claude/CLAUDE.md alongside the user's own
  # global instructions. Marker-bracketed so re-runs only touch our block.
  merge_marker_block "$HOME/.claude/CLAUDE.md" \
    "$ROOT/dist/claude-CLAUDE.md" \
    '<!-- agent-memory:begin do-not-edit-this-block -->' \
    '<!-- agent-memory:end -->'
else
  info "claude CLI not found — skipping (install Claude Code to enable)"
fi

# ─── 4. Codex wiring (CLI or desktop app) ────────────────────────────────
step "4. Codex"
# Symlink the bundled CLI from Codex.app if it's the only Codex on disk
if [[ -x /Applications/Codex.app/Contents/Resources/codex ]] && ! have codex; then
  ln -sf /Applications/Codex.app/Contents/Resources/codex "$PREFIX/bin/codex"
  info "symlinked → $PREFIX/bin/codex"
fi

if have codex; then
  if ! codex mcp list 2>/dev/null | grep -q '^recall'; then
    codex mcp add recall "$RECALL" mcp >/dev/null
    info "MCP server registered → ~/.codex/config.toml"
  else
    info "MCP server already registered"
  fi

  merge_marker_block "$HOME/.codex/AGENTS.md" \
    "$ROOT/dist/codex-AGENTS.md" \
    '<!-- agent-memory:begin do-not-edit-this-block -->' \
    '<!-- agent-memory:end -->'
else
  info "codex CLI not found and no Codex.app present — skipping"
fi

# ─── 5. Cursor wiring ────────────────────────────────────────────────────
step "5. Cursor"
if [[ -d /Applications/Cursor.app ]]; then
  CURSOR_MCP="$HOME/.cursor/mcp.json"
  mkdir -p "$HOME/.cursor"
  if [[ ! -f "$CURSOR_MCP" ]]; then
    cat > "$CURSOR_MCP" <<JSON
{
  "mcpServers": {
    "recall": {
      "command": "$RECALL",
      "args": ["mcp"]
    }
  }
}
JSON
    info "wrote $CURSOR_MCP"
  elif have jq; then
    if ! jq -e '.mcpServers."recall"' "$CURSOR_MCP" >/dev/null 2>&1; then
      tmp=$(mktemp)
      jq --arg cmd "$RECALL" \
         '.mcpServers["recall"] = {command: $cmd, args: ["mcp"]}' \
         "$CURSOR_MCP" > "$tmp"
      mv "$tmp" "$CURSOR_MCP"
      info "merged recall entry into $CURSOR_MCP"
    else
      info "$CURSOR_MCP already has recall entry"
    fi
  else
    warn "$CURSOR_MCP exists and jq not available — add this server by hand:"
    info '  "recall": { "command": "'$RECALL'", "args": ["mcp"] }'
  fi

  warn "Manual step (one-time): paste dist/cursor-user-rule.md body into"
  warn "Cursor → Settings → Rules → User Rules. Cursor's user-rules tier is"
  warn "server-side and configured via the UI — there's no on-disk path."
else
  info "Cursor.app not present — skipping"
fi

# ─── 6. Hermes wiring ────────────────────────────────────────────────────
# Hermes (NousResearch) reads MCP servers from ~/.hermes/config.yaml under the
# `mcp_servers:` key. There's no `hermes mcp add` CLI, so we merge YAML directly
# with yq when available, and fall back to printing the snippet otherwise. We
# never create a partial config: if Hermes hasn't been initialised (no
# config.yaml) we skip, since Hermes generates it from its own template on first
# run. Hermes sessions are *already* ingested into recall from ~/.hermes/state.db
# by the watch daemon — this step adds the reverse direction (Hermes querying
# recall), making the integration bidirectional like the other tools.
step "6. Hermes"
HERMES_CFG="$HOME/.hermes/config.yaml"
if [[ -d "$HOME/.hermes" ]]; then
  if [[ ! -f "$HERMES_CFG" ]]; then
    info "~/.hermes exists but no config.yaml yet — run Hermes once, then re-run setup"
  elif grep -Eq '^[[:space:]]+recall:' "$HERMES_CFG"; then
    info "$HERMES_CFG already has an recall entry"
  elif have yq; then
    RECALL="$RECALL" yq -i '.mcp_servers."recall" = {"command": strenv(RECALL), "args": ["mcp"]}' "$HERMES_CFG" \
      && info "merged recall entry into $HERMES_CFG"
  else
    warn "$HERMES_CFG exists and yq not available — add this under mcp_servers: by hand:"
    info "  recall:"
    info "    command: $RECALL"
    info "    args: [mcp]"
  fi
  warn "Manual step (one-time): add the body of dist/hermes-rule.md to your Hermes"
  warn "system prompt or a skill so Hermes uses recall_* proactively, then run"
  warn "/reload-mcp in Hermes (or restart it)."
else
  info "~/.hermes not present — Hermes not installed; skipping"
fi

# ─── 7. Backfill ingest (pull existing history from every source) ───────
step "7. Backfill: ingest existing Claude Code / Codex / Cursor / Hermes history"
"$RECALL" ingest 2>&1 | sed 's/^/  /'

# ─── 8. Embed captured decisions ─────────────────────────────────────────
step "8. Embed decisions (vectors for semantic recall)"
"$RECALL" embed --scope decisions 2>&1 | sed 's/^/  /'

# ─── 8. Paraphrase (optional, needs API key) ─────────────────────────────
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  step "9. Paraphrase decisions (Haiku alt-phrasings — broadens semantic recall)"
  "$RECALL" paraphrase --limit 50 --per-decision 4 2>&1 | sed 's/^/  /' || \
    warn "paraphrase failed (non-fatal); will retry next time you run it manually"
else
  step "9. Paraphrase (skipped — set ANTHROPIC_API_KEY to enable)"
  info "without it, semantic recall still works — just no Haiku-generated alt-phrasings"
fi

# ─── 10. Watch daemon ────────────────────────────────────────────────────
step "10. Watch daemon (Codex + Cursor + Hermes pickup)"
case "$(uname -s)" in
  Darwin)
    make watch-install >/dev/null
    PLIST="$HOME/Library/LaunchAgents/ai.fantazm.recall.watch.plist"
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load   "$PLIST"
    sleep 1
    if launchctl list | grep -q ai.fantazm.recall.watch; then
      info "loaded — polls every 30 s (launchd)"
    else
      warn "daemon failed to load; check launchctl error"
    fi
    ;;
  Linux)
    if command -v systemctl >/dev/null 2>&1; then
      UNIT_DIR="$HOME/.config/systemd/user"
      mkdir -p "$UNIT_DIR" "$HOME/.local/state/recall"
      sed "s|__BIN__|$RECALL|g" dist/recall-watch.service.template \
        > "$UNIT_DIR/recall-watch.service"
      systemctl --user daemon-reload 2>/dev/null || true
      if systemctl --user enable --now recall-watch.service 2>&1 | sed 's/^/  /'; then
        if systemctl --user is-active --quiet recall-watch.service; then
          info "loaded — polls every 30 s (systemd user unit)"
        else
          warn "unit installed but not active; check 'systemctl --user status recall-watch'"
        fi
      else
        warn "systemctl enable failed; if running headless, try 'loginctl enable-linger $USER'"
      fi
    else
      warn "no systemd detected; run 'recall watch --interval=30s' yourself"
      info "(e.g. a cron @reboot entry) to auto-ingest Codex + Cursor + Hermes"
    fi
    ;;
  *)
    warn "watch-daemon auto-install isn't supported on $(uname -s) yet"
    info "run 'recall watch --interval=30s' manually to auto-ingest Codex + Cursor + Hermes"
    ;;
esac

# ─── 11. Doctor ──────────────────────────────────────────────────────────
step "11. Self-check"
"$RECALL" doctor || true

step "Done"
info "Final manual steps:"
info "  Cursor: open Cursor → Settings → Rules → User Rules → paste body of"
info "    $ROOT/dist/cursor-user-rule.md, then restart Cursor."
info "  Hermes: add the body of $ROOT/dist/hermes-rule.md to your Hermes system"
info "    prompt or a skill, then run /reload-mcp in Hermes (or restart it)."
