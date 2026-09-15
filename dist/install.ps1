<#
.SYNOPSIS
  recall installer for Windows. Downloads the latest signed release zip from
  GitHub, verifies its SHA256, drops recall.exe on PATH, and points you at
  the setup script.

.DESCRIPTION
  Intended to be run as:

      irm https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.ps1 | iex

  Env knobs:
    $env:RECALL_VERSION  pin a specific tag (e.g. v0.2.0); default = latest
    $env:RECALL_FULL     set to 1 to grab the -full zip (bundles the ONNX
                         runtime DLL + model, for airgapped / egress-filtered
                         machines)
    $env:RECALL_PREFIX   install dir; default %USERPROFILE%\.local\bin
#>

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repo = 'zdaniels/recall'

# Only windows-amd64 is published today; it runs natively on x64 and via
# x64 emulation on Windows arm64. (Add a windows/arm64 release job to claim
# a native arm64 build.)
$arch = 'amd64'

$tag = $env:RECALL_VERSION
if (-not $tag) {
  Write-Host "Resolving latest release..."
  $rel = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'recall-install' }
  $tag = $rel.tag_name
}
Write-Host "Installing recall $tag (windows-$arch)..."

$suffix = if ($env:RECALL_FULL -eq '1') { "-full" } else { "" }
$asset  = "recall-$tag-windows-$arch$suffix.zip"
$base   = "https://github.com/$repo/releases/download/$tag"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "recall-$tag"
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$zip = Join-Path $tmp $asset
$shaFile = "$zip.sha256"

Write-Host "Downloading $asset..."
Invoke-WebRequest "$base/$asset" -OutFile $zip
try { Invoke-WebRequest "$base/$asset.sha256" -OutFile $shaFile } catch { $shaFile = $null }

if ($shaFile -and (Test-Path $shaFile)) {
  $want = (Get-Content -Raw $shaFile).Trim().Split()[0].ToLower()
  $got  = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
  if ($want -ne $got) {
    Write-Error "SHA256 mismatch for $asset`n  expected $want`n  got      $got"
    exit 1
  }
  Write-Host "  SHA256 verified"
} else {
  Write-Warning "no .sha256 published for $asset — skipping integrity check"
}

$prefix = if ($env:RECALL_PREFIX) { $env:RECALL_PREFIX } else { Join-Path $env:USERPROFILE ".local\bin" }
New-Item -ItemType Directory -Force -Path $prefix | Out-Null

Write-Host "Extracting to $prefix..."
Expand-Archive -Path $zip -DestinationPath $tmp -Force
# binary-only zip has recall.exe at the root; -full zip too (we stage it flat).
$exe = Get-ChildItem -Path $tmp -Filter recall.exe -Recurse | Select-Object -First 1
if (-not $exe) { Write-Error "recall.exe not found in $asset"; exit 1 }
Copy-Item $exe.FullName (Join-Path $prefix "recall.exe") -Force
# -full zip also carries runtime\ + models\ — copy them next to the binary's
# assets home if present.
foreach ($d in @('runtime', 'models')) {
  $srcDir = Join-Path (Split-Path $exe.FullName) $d
  if (Test-Path $srcDir) {
    $dst = Join-Path $env:USERPROFILE ".local\share\recall\$d"
    New-Item -ItemType Directory -Force -Path (Split-Path $dst) | Out-Null
    Copy-Item $srcDir $dst -Recurse -Force
    Write-Host "  bundled $d -> $dst"
  }
}

# Put $prefix on the user PATH if it isn't already.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$prefix*") {
  [Environment]::SetEnvironmentVariable('Path', "$prefix;$userPath", 'User')
  Write-Host "  added $prefix to your user PATH (restart the shell to pick it up)"
}

Write-Host ""
Write-Host "recall installed -> $(Join-Path $prefix 'recall.exe')"
Write-Host "Next:"
if ($env:RECALL_FULL -ne '1') {
  Write-Host "  recall download-model      # one-time, ~57 MiB, SHA256-verified"
}
Write-Host "  recall doctor              # sanity check"
Write-Host "  # then wire your tools:  pwsh -File <recall repo>\dist\setup.ps1"
