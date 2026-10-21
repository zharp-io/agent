#!/usr/bin/env bash
# Zharp Collector — installer
#
# With an API key it installs, finds what runs on this server and starts collecting — no questions:
#   curl -fsSL https://raw.githubusercontent.com/zharp-io/agent/main/install.sh | sudo bash -s -- --api-key zh_...
# Without one it installs the binary and points at the guided setup.
#
# On a server where the collector is already set up (its config exists) it updates instead: the
# binary is replaced and `zharp-collector update --apply` adds what the new version detects,
# keeping the config, API key and everything already collected. --api-key is not needed then.
set -euo pipefail

REPO="zharp-io/agent"
INSTALL_DIR="/usr/local/bin"
CONFIG_FILE="/etc/zharp-collector/config.yaml"

API_KEY="${ZHARP_API_KEY:-}"
ENDPOINT="${ZHARP_ENDPOINT:-}"
NO_APP_LOGS="${ZHARP_NO_APP_LOGS:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key|--token) API_KEY="$2"; shift 2 ;;
    --endpoint|--server) ENDPOINT="$2"; shift 2 ;;
    --version) ZHARP_VERSION="$2"; shift 2 ;;
    --no-app-logs) NO_APP_LOGS=1; shift ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

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

EXISTING=""
[[ -f "$CONFIG_FILE" ]] && EXISTING=1
if [[ -n "$EXISTING" && "$(id -u)" != "0" ]]; then
  echo "  Updating the installed collector needs root. Run this again with sudo." >&2
  exit 1
fi

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
NEW_BIN="$TMP/zharp-collector-${OS}-${ARCH}"
"$NEW_BIN" --version >/dev/null 2>&1 || { echo "  The downloaded binary does not run on this machine; nothing was changed." >&2; exit 1; }

# The version being replaced, and a copy to put back if the new one can't load the config.
OLD_VERSION=""
if [[ -n "$EXISTING" && -x "$INSTALL_DIR/zharp-collector" ]]; then
  OLD_VERSION="$("$INSTALL_DIR/zharp-collector" --version 2>/dev/null | awk '{print $NF}')" || true
  cp -p "$INSTALL_DIR/zharp-collector" "$TMP/previous" || true
fi

# Copy next to the target, then rename over it: a running binary is never written into.
mkdir -p "$INSTALL_DIR"
install -m 755 "$NEW_BIN" "$INSTALL_DIR/.zharp-collector.new"
mv -f "$INSTALL_DIR/.zharp-collector.new" "$INSTALL_DIR/zharp-collector"
echo "  Installed → $INSTALL_DIR/zharp-collector"

# ── existing install: update it ───────────────────────────────────────────────
if [[ -n "$EXISTING" ]]; then
  echo
  echo "  Found an existing install ($CONFIG_FILE): updating it."
  echo "  The config, API key and everything already collected are kept; new sources are added."
  [[ -n "$API_KEY" ]] && echo "  (--api-key is not used: the key already configured stays.)"
  ARGS=(update --apply --from "${OLD_VERSION:-unknown}")
  case "$NO_APP_LOGS" in 1|true|yes|on) ARGS+=(--no-app-logs) ;; esac
  if ! "$INSTALL_DIR/zharp-collector" "${ARGS[@]}"; then
    if [[ -f "$TMP/previous" ]]; then
      install -m 755 "$TMP/previous" "$INSTALL_DIR/.zharp-collector.new"
      mv -f "$INSTALL_DIR/.zharp-collector.new" "$INSTALL_DIR/zharp-collector"
      echo "  The update was not applied; ${OLD_VERSION:-the previous version} was put back." >&2
    fi
    exit 1
  fi
  exit 0
fi

# ── set up ────────────────────────────────────────────────────────────────────
if [[ -n "$API_KEY" ]]; then
  if [[ "$(id -u)" != "0" ]]; then
    echo "  Setting up the service needs root. Run: sudo $INSTALL_DIR/zharp-collector install --yes --api-key <key>" >&2
    exit 1
  fi
  ARGS=(install --yes --api-key "$API_KEY")
  [[ -n "$ENDPOINT" ]] && ARGS+=(--endpoint "$ENDPOINT")
  case "$NO_APP_LOGS" in 1|true|yes|on) ARGS+=(--no-app-logs) ;; esac
  exec "$INSTALL_DIR/zharp-collector" "${ARGS[@]}"
fi

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
