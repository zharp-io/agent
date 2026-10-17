# Zharp Collector — Windows installer
# Downloads the binary and prints next steps.
#
# Run as Administrator:
#   irm https://raw.githubusercontent.com/zharp-io/agent/main/setup.ps1 | iex
#
# Or download and run locally:
#   Set-ExecutionPolicy Bypass -Scope Process -Force
#   .\setup.ps1

#Requires -RunAsAdministrator
$ErrorActionPreference = "Stop"

$REPO        = "zharp-io/agent"
$INSTALL_DIR = "C:\Program Files\zharp-collector"
$EXE_PATH    = "$INSTALL_DIR\zharp-collector.exe"

# ── resolve version ──────────────────────────────────────────────────────────
$VERSION = $env:ZHARP_VERSION
if (-not $VERSION) {
    Write-Host "  Resolving latest release..."
    $rel = Invoke-RestMethod "https://api.github.com/repos/$REPO/releases/latest"
    $VERSION = $rel.tag_name
}
if (-not $VERSION) {
    Write-Host "Could not resolve version. Set `$env:ZHARP_VERSION=v1.x.x and retry." -ForegroundColor Red
    exit 1
}

# ── download binary ──────────────────────────────────────────────────────────
$ASSET = "zharp-collector-$VERSION-windows-amd64.exe"
$URL   = "https://github.com/$REPO/releases/download/$VERSION/$ASSET"

Write-Host "  Downloading zharp-collector $VERSION..."
New-Item -ItemType Directory -Force -Path $INSTALL_DIR | Out-Null
Invoke-WebRequest -Uri $URL -OutFile $EXE_PATH -UseBasicParsing
Write-Host "  Installed -> $EXE_PATH"

# ── done ────────────────────────────────────────────────────────────────────
Write-Host
Write-Host "  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
Write-Host "    zharp-collector $VERSION installed."
Write-Host "  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
Write-Host
Write-Host "  Run the setup wizard in an elevated terminal:"
Write-Host
Write-Host "    zharp-collector install"
Write-Host
