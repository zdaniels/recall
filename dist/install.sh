#!/usr/bin/env bash
#
# recall — one-line installer (macOS + Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.sh | sh
#
# Downloads the latest release tarball for your platform, verifies its
# SHA256, drops the `recall` binary in ~/.local/bin, and points you at
# the next step. Idempotent; safe to re-run. (Windows: use install.ps1.)
#
# This file is the source for the hosted https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.sh —
# publish it to the site after changes.
#
# Honors:
#   RECALL_VERSION   — install a specific tag instead of latest (e.g. v0.1.0)
#   RECALL_PREFIX    — install prefix (default: ~/.local). Binary goes at $PREFIX/bin/recall.
#   RECALL_REPO      — override the GH repo (default: fantazmai/recall)
#   RECALL_FULL=1    — install the -full tarball (bundles ONNX runtime +
#                      model, ~80 MiB total). For airgapped / restricted
#                      egress installs. Without this you get the slim
#                      tarball and recall downloads the model on first use.

set -euo pipefail

REPO="${RECALL_REPO:-fantazmai/recall}"
PREFIX="${RECALL_PREFIX:-$HOME/.local}"
VERSION="${RECALL_VERSION:-}"
WANT_FULL="${RECALL_FULL:-0}"

cyan() { printf '\033[36m%s\033[0m\n' "$*"; }
red()  { printf '\033[31m%s\033[0m\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
die()  { red "✗ $*"; exit 1; }

# Detect platform → release-tarball suffix. macOS (arm64) and Linux (x64 /
# arm64) ship prebuilt binaries; Windows uses the PowerShell installer.
# Intel Mac and anything else: build from source.
detect_platform() {
  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"
  case "$os/$arch" in
    Darwin/arm64)               echo "darwin-arm64" ;;
    Darwin/x86_64)              die "Intel Macs aren't built yet (Apple Silicon only). Build from source: https://github.com/$REPO" ;;
    Linux/x86_64)               echo "linux-amd64" ;;
    Linux/aarch64 | Linux/arm64) echo "linux-arm64" ;;
    *MINGW* | *MSYS* | *CYGWIN*) die "On Windows, use the PowerShell installer: irm https://raw.githubusercontent.com/zdaniels/recall/main/dist/install.ps1 | iex" ;;
    *)                          die "Unsupported platform: $os/$arch. Build from source: https://github.com/$REPO" ;;
  esac
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1"
}

# sha256 verification works with either shasum (macOS) or sha256sum (Linux).
verify_sha() {
  local dir="$1" file="$2"
  ( cd "$dir" && if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 -c "$file.sha256"
    else
      sha256sum -c "$file.sha256"
    fi ) || die "SHA256 check failed."
}

resolve_version() {
  if [ -n "$VERSION" ]; then
    echo "$VERSION"; return
  fi
  # Latest release via GitHub API. Falls back to the redirect target of
  # /releases/latest if jq isn't available.
  local tag
  if command -v jq >/dev/null 2>&1; then
    tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | jq -r '.tag_name')"
  else
    tag="$(curl -fsSI "https://github.com/$REPO/releases/latest" | grep -i '^location:' | sed -E 's|.*/tag/([^[:space:]]+).*|\1|')"
  fi
  [ -n "$tag" ] && [ "$tag" != "null" ] || die "Could not resolve latest release for $REPO"
  echo "$tag"
}

main() {
  require_cmd curl
  require_cmd tar

  local platform tag tarball_name url_base tarball_url sha_url tmp
  platform="$(detect_platform)"
  tag="$(resolve_version)"

  if [ "$WANT_FULL" = "1" ]; then
    tarball_name="recall-${tag}-${platform}-full.tar.gz"
  else
    tarball_name="recall-${tag}-${platform}.tar.gz"
  fi
  url_base="https://github.com/$REPO/releases/download/$tag"
  tarball_url="$url_base/$tarball_name"
  sha_url="$url_base/$tarball_name.sha256"

  tmp="$(mktemp -d)"
  # Bake the path into the trap (double quotes expand $tmp now): the EXIT trap
  # fires after main() returns, where the `local` tmp is out of scope and
  # `set -u` would otherwise abort with "tmp: unbound variable".
  trap "rm -rf '$tmp'" EXIT

  cyan "recall — installer"
  printf '  repo:     %s\n' "$REPO"
  printf '  tag:      %s\n' "$tag"
  printf '  platform: %s\n' "$platform"
  printf '  prefix:   %s\n' "$PREFIX"
  printf '  tarball:  %s\n\n' "$tarball_name"

  cyan "Downloading…"
  curl -fL --progress-bar "$tarball_url" -o "$tmp/$tarball_name"
  curl -fsSL "$sha_url"                  -o "$tmp/$tarball_name.sha256"

  cyan "Verifying SHA256…"
  verify_sha "$tmp" "$tarball_name"
  ok "checksum matches"

  cyan "Extracting…"
  mkdir -p "$PREFIX/bin"
  tar -xzf "$tmp/$tarball_name" -C "$tmp"
  # The slim tarball ships just `recall`; the -full tarball has recall
  # plus runtime/ and models/ subdirs. Move what's there.
  install -m 755 "$tmp/recall" "$PREFIX/bin/recall"
  ok "installed $PREFIX/bin/recall"
  if [ "$WANT_FULL" = "1" ]; then
    mkdir -p "$HOME/.local/share/recall"
    cp -R "$tmp/runtime" "$HOME/.local/share/recall/"
    cp -R "$tmp/models"  "$HOME/.local/share/recall/"
    ok "installed bundled ONNX runtime + model to ~/.local/share/recall/"
  fi

  case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) printf '\n\033[33m! %s/bin is not on your PATH.\033[0m\n' "$PREFIX"
       printf '  Add this to your shell rc:\n'
       printf '    export PATH="%s/bin:$PATH"\n\n' "$PREFIX" ;;
  esac

  cyan "Next steps:"
  printf '  1. %s download-model        # one-time, ~57 MiB, SHA256-verified\n' "$PREFIX/bin/recall"
  if [ "$WANT_FULL" = "1" ]; then
    printf '     (skipped — bundled in the -full tarball)\n'
  fi
  printf '  2. %s doctor                # self-check\n' "$PREFIX/bin/recall"
  printf '  3. Start a new Claude Code / Codex / Cursor session — recall auto-injects.\n\n'
  printf '  See https://github.com/zdaniels/recall for the full setup walkthrough.\n'
}

main "$@"
