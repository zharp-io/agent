# The agent installed for real on Windows, the way a customer runs it: install, an update's merge
# step, uninstall. Run from the repository root with the binary at .\zharp-collector.exe, as an
# administrator (GitHub's Windows runners are).
#
# The API key and endpoint are dummies: nothing is sent anywhere, which is fine — what's checked
# is that the service installs, starts, stays up through an update and is removed.
$ErrorActionPreference = 'Stop'
$bin = Join-Path (Get-Location) 'zharp-collector.exe'

function Run([string[]]$argv) {
    $out = & $bin @argv 2>&1 | Out-String
    Write-Output $out
    if ($LASTEXITCODE -ne 0) { throw "zharp-collector $($argv -join ' ') exited with $LASTEXITCODE" }
    return $out
}

function Assert-Running([string]$when) {
    # A large binary takes a while to start on Windows (Defender scans it first): give it a minute.
    $deadline = (Get-Date).AddSeconds(60)
    do {
        $state = (sc.exe query ZharpCollector | Select-String 'STATE').ToString()
        if ($state -match 'RUNNING') { return }
        Start-Sleep -Seconds 1
    } while ((Get-Date) -lt $deadline)
    throw "the service isn't running $when`: $state"
}

Run @('--version') | Out-Null
$install = Run @('install', '--api-key', 'zh_ci_dummy', '--endpoint', 'http://127.0.0.1:9/api/v1', '-y')
if ($install -match 'may not have started') { throw 'install reported the service may not have started' }
Assert-Running 'after install'

$config = Join-Path $env:PROGRAMDATA 'zharp-collector\config.yaml'
if (-not (Select-String -Path $config -Pattern '^  hostmetrics/processes:' -Quiet)) { throw 'processes are not collected' }
# Disk and network by program are Linux only: not in a Windows config.
if (Select-String -Path $config -Pattern 'zharp_disk_usage|zharp_network_usage' -Quiet) { throw 'Linux-only receivers in a Windows config' }

# An update's merge step, which restarts the service.
$update = Run @('update', '--apply', '--from', 'v1.9.4')
if ($update -match 'Restart failed') { throw 'the update could not restart the service' }
Assert-Running 'after the update'

'y' | & $bin uninstall
if ($LASTEXITCODE -ne 0) { throw "uninstall exited with $LASTEXITCODE" }
if (Get-Service ZharpCollector -ErrorAction SilentlyContinue) { throw 'the service is still there after uninstall' }
Write-Output 'smoke test passed on Windows'
