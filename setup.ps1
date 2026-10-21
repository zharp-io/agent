# Zharp Collector — Windows installer
#
# Run as Administrator. With an API key it installs, finds what runs on this server and starts
# collecting — no questions:
#   $env:ZHARP_API_KEY="zh_..."; irm https://raw.githubusercontent.com/zharp-io/agent/main/setup.ps1 | iex
# Without one it installs the binary and points at the guided setup:
#   irm https://raw.githubusercontent.com/zharp-io/agent/main/setup.ps1 | iex
#
# Or download and run locally:
#   Set-ExecutionPolicy Bypass -Scope Process -Force
#   .\setup.ps1
#
# On a machine where the collector is already set up (its config exists) it updates instead: the
# binary is replaced and `zharp-collector update --apply` adds what the new version detects,
# keeping the config, API key and everything already collected. No API key is needed then.

#Requires -RunAsAdministrator
$ErrorActionPreference = "Stop"

$REPO        = "zharp-io/agent"
$INSTALL_DIR = "C:\Program Files\zharp-collector"
$EXE_PATH    = "$INSTALL_DIR\zharp-collector.exe"
$PROGRAM_DATA = if ($env:PROGRAMDATA) { $env:PROGRAMDATA } else { "C:\ProgramData" }
$CONFIG_FILE = "$PROGRAM_DATA\zharp-collector\config.yaml"
$EXISTING    = Test-Path $CONFIG_FILE

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
$NEW_EXE = "$INSTALL_DIR\zharp-collector.new.exe"
Invoke-WebRequest -Uri $URL -OutFile $NEW_EXE -UseBasicParsing

# A running .exe can't be overwritten, but it can be renamed: the old one is set aside (and put
# back if the new one can't load the config). The collector removes it on its next start.
$OLD_VERSION = ""
$OLD_EXE = ""
if (Test-Path $EXE_PATH) {
    if ($EXISTING) {
        $OLD_VERSION = ((& $EXE_PATH --version) -split ' ')[-1]
    }
    $OLD_EXE = "$EXE_PATH.old"
    Remove-Item $OLD_EXE -Force -ErrorAction SilentlyContinue
    if (Test-Path $OLD_EXE) { $OLD_EXE = "$EXE_PATH.old-$([DateTime]::UtcNow.Ticks)" }
    Move-Item $EXE_PATH $OLD_EXE -Force
}
Move-Item $NEW_EXE $EXE_PATH -Force
Write-Host "  Installed -> $EXE_PATH"

# ── existing install: update it ──────────────────────────────────────────────
if ($EXISTING) {
    Write-Host
    Write-Host "  Found an existing install ($CONFIG_FILE): updating it."
    Write-Host "  The config, API key and everything already collected are kept; new sources are added."
    if (-not $OLD_VERSION) { $OLD_VERSION = "unknown" }
    # ZHARP_NO_APP_LOGS, when set, reaches it through the environment.
    $updateArgs = @("update", "--apply", "--from", $OLD_VERSION)
    & $EXE_PATH @updateArgs
    if ($LASTEXITCODE -ne 0 -and $OLD_EXE) {
        Remove-Item $EXE_PATH -Force
        Move-Item $OLD_EXE $EXE_PATH -Force
        Write-Host "  The update was not applied; $OLD_VERSION was put back." -ForegroundColor Yellow
    }
    return
}

# ── set up ──────────────────────────────────────────────────────────────────
if ($env:ZHARP_API_KEY) {
    $setupArgs = @("install", "--yes", "--api-key", $env:ZHARP_API_KEY)
    if ($env:ZHARP_ENDPOINT) { $setupArgs += @("--endpoint", $env:ZHARP_ENDPOINT) }
    & $EXE_PATH @setupArgs
    # return, not exit: under `irm | iex` exit would close the window it runs in.
    return
}

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
