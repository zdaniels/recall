<#
.SYNOPSIS
  Recall bootstrap for Windows — one-shot, idempotent. PowerShell port of
  dist/setup.sh.

.DESCRIPTION
  Wires every supported tool found on this machine:
    - Claude Code  -> MCP register + SessionStart/Stop/PostToolUse/PreCompact
                      hooks + %USERPROFILE%\.claude\CLAUDE.md rule block
    - Codex CLI    -> MCP register + %USERPROFILE%\.codex\AGENTS.md drop-in
    - Cursor       -> %USERPROFILE%\.cursor\mcp.json (User Rule paste is manual)
    - watch daemon -> a logon Scheduled Task running `recall watch`
    - ONNX model   -> recall download-model (~57 MiB, SHA256-verified)

  Re-run safe — every step checks state before acting and skips cleanly if a
  tool isn't installed.

  Requires PowerShell 7+ (pwsh). On Windows PowerShell 5.1 the JSON helpers
  used here (ConvertFrom-Json -AsHashtable) aren't available.
#>

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

if ($PSVersionTable.PSVersion.Major -lt 7) {
  Write-Error "PowerShell 7+ required (you have $($PSVersionTable.PSVersion)). Install via 'winget install Microsoft.PowerShell' and re-run with pwsh."
  exit 1
}

$Root   = Split-Path -Parent $PSScriptRoot
$Home_  = $env:USERPROFILE
$BinDir = Join-Path $Home_ ".local\bin"
$Recall = Join-Path $BinDir "recall.exe"
$StateDir = Join-Path $Home_ ".local\state\recall"

function Step($m) { Write-Host "`n> $m" -ForegroundColor White }
function Info($m) { Write-Host "  $m" }
function Warn($m) { Write-Host "  ! $m" -ForegroundColor Yellow }
function Have($cmd) { [bool](Get-Command $cmd -ErrorAction SilentlyContinue) }

# merge_marker_block — drop the contents of $SourceFile into $Target, bracketed
# by $Begin/$End. Re-runs replace only the content between the markers; anything
# outside is preserved. Mirrors the bash awk version's four cases.
function Merge-MarkerBlock($Target, $SourceFile, $Begin, $End) {
  $src = Get-Content -Raw -LiteralPath $SourceFile
  $block = "$Begin`n$src`n$End`n"
  $dir = Split-Path -Parent $Target
  if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

  if (-not (Test-Path $Target)) {
    Set-Content -LiteralPath $Target -Value $block -NoNewline
    Info "wrote $Target"
    return
  }
  $cur = Get-Content -Raw -LiteralPath $Target
  if ($cur.Contains($Begin)) {
    # Literal splice: replace everything from $Begin..$End inclusive with the
    # fresh block, leaving surrounding content untouched.
    $bi = $cur.IndexOf($Begin)
    $ei = $cur.IndexOf($End, $bi)
    if ($ei -ge 0) {
      $new = $cur.Substring(0, $bi) + "$Begin`n$src`n$End" + $cur.Substring($ei + $End.Length)
      if ($new -ne $cur) {
        Set-Content -LiteralPath $Target -Value $new -NoNewline
        Info "recall block updated in $Target (other content preserved)"
      } else {
        Info "$Target already up to date"
      }
    }
  } elseif ($cur.TrimEnd() -eq $src.TrimEnd()) {
    Set-Content -LiteralPath $Target -Value $block -NoNewline
    Info "$Target migrated to use recall markers (content unchanged)"
  } else {
    Warn "$Target exists without recall markers — leaving it alone."
    Warn "Wrap your block with these markers so re-runs update it:"
    Info "  $Begin"
    Info "  $End"
  }
}

# ── 1. Build + install the binary ─────────────────────────────────────────
Step "1. Build + install recall"
if (-not (Test-Path $Recall)) {
  if (Have go) {
    New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
    Push-Location $Root
    & go build -trimpath -o $Recall ./cmd/recall
    Pop-Location
    Info "built -> $Recall"
  } else {
    Write-Error "recall.exe not found at $Recall and Go isn't installed to build it. Install from https://github.com/zdaniels/recall first."
    exit 1
  }
} else {
  Info "already installed -> $Recall ($(& $Recall version 2>&1))"
}
$env:Path = "$BinDir;$env:Path"

# ── 2. Download ONNX model + assets ───────────────────────────────────────
Step "2. Download ONNX model (idempotent, SHA256-verified)"
& $Recall download-model

# ── 3. Claude Code wiring ─────────────────────────────────────────────────
Step "3. Claude Code"
if (Have claude) {
  $mcpList = (& claude mcp list 2>$null) -join "`n"
  if ($mcpList -notmatch '(?m)^recall') {
    & claude mcp add --scope user recall $Recall mcp | Out-Null
    Info "MCP server registered -> ~/.claude.json"
  } else {
    Info "MCP server already registered"
  }

  $settingsPath = Join-Path $Home_ ".claude\settings.json"
  New-Item -ItemType Directory -Force -Path (Split-Path $settingsPath) | Out-Null

  $hookInject     = "recall inject --prompt-stdin --budget 1500"
  $hookTool       = "recall record-tool-event"
  $hookStop       = "recall ingest"
  $hookPrecompact = "recall ingest"

  $settings = @{}
  if (Test-Path $settingsPath) {
    $raw = Get-Content -Raw -LiteralPath $settingsPath
    if ($raw.Trim()) { $settings = $raw | ConvertFrom-Json -AsHashtable }
  }
  if (-not $settings.ContainsKey('hooks')) { $settings['hooks'] = @{} }
  $hooks = $settings['hooks']

  function Add-Hook($event, $entry, $cmd) {
    $arr = @()
    if ($hooks.ContainsKey($event)) { $arr = @($hooks[$event]) }
    $already = $false
    foreach ($e in $arr) {
      $json = ($e | ConvertTo-Json -Depth 20 -Compress)
      if ($json -like "*$cmd*") { $already = $true; break }
    }
    if (-not $already) { $arr += $entry }
    $hooks[$event] = $arr
  }

  Add-Hook 'SessionStart' (@{ hooks = @(@{ type = 'command'; command = $hookInject }) }) $hookInject
  Add-Hook 'PostToolUse'  (@{ matcher = 'Bash'; hooks = @(@{ type = 'command'; command = $hookTool }) }) $hookTool
  Add-Hook 'Stop'         (@{ hooks = @(@{ type = 'command'; command = $hookStop }) }) $hookStop
  Add-Hook 'PreCompact'   (@{ hooks = @(@{ type = 'command'; command = $hookPrecompact }) }) $hookPrecompact

  $settings['hooks'] = $hooks
  ($settings | ConvertTo-Json -Depth 20) | Set-Content -LiteralPath $settingsPath
  Info "hooks merged into $settingsPath (existing hooks preserved)"

  Merge-MarkerBlock (Join-Path $Home_ ".claude\CLAUDE.md") `
    (Join-Path $Root "dist\claude-CLAUDE.md") `
    '<!-- agent-memory:begin do-not-edit-this-block -->' `
    '<!-- agent-memory:end -->'
} else {
  Info "claude CLI not found — skipping (install Claude Code to enable)"
}

# ── 4. Codex wiring ───────────────────────────────────────────────────────
Step "4. Codex"
if (Have codex) {
  $codexMcp = (& codex mcp list 2>$null) -join "`n"
  if ($codexMcp -notmatch '(?m)^recall') {
    & codex mcp add recall $Recall mcp | Out-Null
    Info "MCP server registered -> ~/.codex/config.toml"
  } else {
    Info "MCP server already registered"
  }
  Merge-MarkerBlock (Join-Path $Home_ ".codex\AGENTS.md") `
    (Join-Path $Root "dist\codex-AGENTS.md") `
    '<!-- agent-memory:begin do-not-edit-this-block -->' `
    '<!-- agent-memory:end -->'
} else {
  Info "codex CLI not found — skipping"
}

# ── 5. Cursor wiring ──────────────────────────────────────────────────────
Step "5. Cursor"
$cursorMcp = Join-Path $Home_ ".cursor\mcp.json"
if (Test-Path (Join-Path $env:LOCALAPPDATA "Programs\cursor")) {
  New-Item -ItemType Directory -Force -Path (Split-Path $cursorMcp) | Out-Null
  if (-not (Test-Path $cursorMcp)) {
    @{ mcpServers = @{ 'recall' = @{ command = $Recall; args = @('mcp') } } } |
      ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $cursorMcp
    Info "wrote $cursorMcp"
  } else {
    $cfg = (Get-Content -Raw -LiteralPath $cursorMcp) | ConvertFrom-Json -AsHashtable
    if (-not $cfg.ContainsKey('mcpServers')) { $cfg['mcpServers'] = @{} }
    if (-not $cfg['mcpServers'].ContainsKey('recall')) {
      $cfg['mcpServers']['recall'] = @{ command = $Recall; args = @('mcp') }
      ($cfg | ConvertTo-Json -Depth 20) | Set-Content -LiteralPath $cursorMcp
      Info "merged recall entry into $cursorMcp"
    } else {
      Info "$cursorMcp already has recall entry"
    }
  }
  Warn "Manual step (one-time): paste dist\cursor-user-rule.md body into"
  Warn "Cursor -> Settings -> Rules -> User Rules (server-side, no on-disk path)."
} else {
  Info "Cursor not present — skipping"
}

# ── 6. Backfill ingest ────────────────────────────────────────────────────
Step "6. Backfill: ingest existing Claude Code / Codex / Cursor history"
& $Recall ingest

# ── 7. Embed captured decisions ───────────────────────────────────────────
Step "7. Embed decisions (vectors for semantic recall)"
& $Recall embed --scope decisions

# ── 8. Paraphrase (optional, needs API key) ───────────────────────────────
if ($env:ANTHROPIC_API_KEY) {
  Step "8. Paraphrase decisions (Haiku alt-phrasings)"
  try { & $Recall paraphrase --limit 50 --per-decision 4 }
  catch { Warn "paraphrase failed (non-fatal); retry manually later" }
} else {
  Step "8. Paraphrase (skipped — set ANTHROPIC_API_KEY to enable)"
  Info "semantic recall still works without it"
}

# ── 9. Watch daemon (Scheduled Task at logon) ─────────────────────────────
Step "9. Watch daemon (Codex + Cursor pickup)"
New-Item -ItemType Directory -Force -Path $StateDir | Out-Null
$taskName = "RecallWatch"
try {
  $action  = New-ScheduledTaskAction -Execute $Recall -Argument "watch --interval=30s"
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $settings2 = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
  Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings2 -Force | Out-Null
  Start-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
  Info "registered scheduled task '$taskName' — runs at logon, polls every 30 s"
} catch {
  Warn "could not register the scheduled task: $($_.Exception.Message)"
  Info "run 'recall watch --interval=30s' manually to auto-ingest Codex + Cursor"
}

# ── 10. Doctor ────────────────────────────────────────────────────────────
Step "10. Self-check"
try { & $Recall doctor } catch { }

Step "Done"
Info "Final manual step (Cursor only): paste dist\cursor-user-rule.md into"
Info "Cursor -> Settings -> Rules -> User Rules, then restart Cursor."
