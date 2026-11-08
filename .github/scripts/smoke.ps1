# The agent installed for real on Windows, the way a customer runs it: install, an update's merge
# step, uninstall. Run from the repository root with the binary at .\zharp-collector.exe, as an
# administrator (GitHub's Windows runners are).
#
# The API key and endpoint are dummies: nothing is sent anywhere, which is fine — what's checked
# is that the service installs, starts, stays up and is removed.
$ErrorActionPreference = 'Stop'
$bin = Join-Path (Get-Location) 'zharp-collector.exe'

function Run([string[]]$argv) {
    & $bin @argv
    if ($LASTEXITCODE -ne 0) { throw "zharp-collector $($argv -join ' ') exited with $LASTEXITCODE" }
}

Run @('--version')
Run @('install', '--api-key', 'zh_ci_dummy', '--endpoint', 'http://127.0.0.1:9/api/v1', '-y')
Start-Sleep -Seconds 8
Run @('status')

$config = Join-Path $env:PROGRAMDATA 'zharp-collector\config.yaml'
if (-not (Select-String -Path $config -Pattern '^  hostmetrics/processes:' -Quiet)) { throw 'processes are not collected' }
# Disk and network by program are Linux only: not in a Windows config.
if (Select-String -Path $config -Pattern 'zharp_disk_usage|zharp_network_usage' -Quiet) { throw 'Linux-only receivers in a Windows config' }

Run @('update', '--apply', '--from', 'v1.9.4')
Start-Sleep -Seconds 5
Run @('status')

'y' | & $bin uninstall
if ($LASTEXITCODE -ne 0) { throw "uninstall exited with $LASTEXITCODE" }
Write-Output 'smoke test passed on Windows'
