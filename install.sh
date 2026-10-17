#!/usr/bin/env bash
# Zharp Collector — installer
# Downloads the binary and prints next steps.
set -euo pipefail

REPO="zharp-io/agent"
INSTALL_DIR="/usr/local/bin"

# ── platform ──────────────────────────────────────────────────────────────────
_UNAME="$(uname -s)"
OS="$(echo "$_UNAME" | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# On Windows under Git Bash / MSYS2 / Cygwin, hand off to the PowerShell setup.
if [[ "$_UNAME" =~ MINGW|MSYS|CYGWIN ]]; then
  echo
  echo "  Windows detected — launching PowerShell setup..."
  echo
  PS1_LOCAL="$(dirname "$0")/setup.ps1"
  if [[ -f "$PS1_LOCAL" ]]; then
    powershell.exe -ExecutionPolicy Bypass -NoProfile -File "$(cygpath -w "$PS1_LOCAL")"
  else
    powershell.exe -ExecutionPolicy Bypass -NoProfile \
      -Command "irm https://raw.githubusercontent.com/$REPO/main/setup.ps1 | iex"
  fi
  exit $?
fi

[[ "$OS" != "linux" && "$OS" != "darwin" ]] && {
  echo "Unsupported OS: $_UNAME" >&2; exit 1; }

if command -v curl &>/dev/null;   then FETCH="curl -sSfL"
elif command -v wget &>/dev/null; then FETCH="wget -qO-"
else echo "curl or wget is required." >&2; exit 1; fi

# ── resolve version ───────────────────────────────────────────────────────────
VERSION="${ZHARP_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  echo "  Resolving latest release..."
  VERSION=$($FETCH "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\(.*\)".*/\1/')
fi
[[ -z "$VERSION" ]] && { echo "Could not resolve version. Set ZHARP_VERSION=v1.x.x and retry." >&2; exit 1; }

# ── download binary ───────────────────────────────────────────────────────────
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "  Downloading zharp-collector $VERSION ($OS/$ARCH)..."
ARCHIVE="zharp-collector-${VERSION}-${OS}-${ARCH}.tar.gz"
$FETCH "https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE" -o "$TMP/$ARCHIVE"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
install -m 755 "$TMP/zharp-collector-${OS}-${ARCH}" "$INSTALL_DIR/zharp-collector"
echo "  Installed → $INSTALL_DIR/zharp-collector"

# ── done ──────────────────────────────────────────────────────────────────────
echo
echo "  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "    zharp-collector $VERSION installed."
echo "  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo
echo "  Run the setup wizard:"
echo
echo "    sudo zharp-collector install"
echo
