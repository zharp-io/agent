//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const winSvcName = "ZharpCollector"

// svcInstall installs and starts the Windows service.
func svcInstall(cfg *svcInstallConfig) error {
	// Stop and delete existing service (ignore errors).
	exec.Command("sc.exe", "stop", winSvcName).Run()   //nolint:errcheck
	exec.Command("sc.exe", "delete", winSvcName).Run() //nolint:errcheck

	// Poll until the registry key is gone (Windows deletes it asynchronously).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE,
			`SYSTEM\CurrentControlSet\Services\`+winSvcName, registry.QUERY_VALUE)
		if err != nil {
			break // key gone
		}
		k.Close()
		time.Sleep(500 * time.Millisecond)
	}

	// Add SYSTEM to docker-users if needed.
	if cfg.hasDocker {
		exec.Command("powershell", "-NoProfile", "-Command", //nolint:errcheck
			`Add-LocalGroupMember -Group 'docker-users' -Member 'NT AUTHORITY\SYSTEM' -ErrorAction SilentlyContinue`,
		).Run()
	}

	// Create service.
	binPathName := fmt.Sprintf(`"%s" --config "file:%s"`, cfg.binaryPath, cfg.configFile)
	createCmd := fmt.Sprintf(
		`New-Service -Name '%s' -DisplayName 'Zharp Collector' -Description 'Zharp OpenTelemetry Collector agent' -BinaryPathName '%s' -StartupType Automatic | Out-Null`,
		winSvcName, strings.ReplaceAll(binPathName, "'", "''"),
	)
	if out, err := exec.Command("powershell", "-NoProfile", "-Command", createCmd).CombinedOutput(); err != nil {
		return fmt.Errorf("create service: %w: %s", err, out)
	}

	// Set failure actions.
	exec.Command("sc.exe", "failure", winSvcName, "reset=", "86400", //nolint:errcheck
		"actions=", "restart/5000/restart/5000/restart/5000").Run()

	// Write env vars to registry as REG_MULTI_SZ.
	regPath := `SYSTEM\CurrentControlSet\Services\` + winSvcName
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open service registry key: %w", err)
	}
	defer k.Close()

	envEntries := make([]string, 0, len(cfg.envVars))
	for key, val := range cfg.envVars {
		envEntries = append(envEntries, key+"="+val)
	}
	if err := k.SetStringsValue("Environment", envEntries); err != nil {
		return fmt.Errorf("set registry Environment: %w", err)
	}

	// Start the service.
	if out, err := exec.Command("sc.exe", "start", winSvcName).CombinedOutput(); err != nil {
		return fmt.Errorf("start service: %w: %s", err, out)
	}

	// Verify. A large binary takes a while to start (and an antivirus may scan it first), so a
	// starting service is given time before it's reported as not started.
	if waitForServiceState("RUNNING", 30*time.Second) {
		uiOK("Service started.")
	} else {
		uiWarn("Service may not have started. Check logs:")
		uiDimMsg(`Get-EventLog -LogName Application -Source '*zharp*' -Newest 20 | Format-List`)
	}
	return nil
}

// svcStart starts the Windows service.
func svcStart() error {
	return exec.Command("sc.exe", "start", winSvcName).Run()
}

// svcStop stops the Windows service.
func svcStop() error {
	return exec.Command("sc.exe", "stop", winSvcName).Run()
}

// svcStatus shows the Windows service status.
func svcStatus() error {
	cmd := exec.Command("sc.exe", "query", winSvcName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// svcLogs shows recent event log entries for the collector.
func svcLogs() error {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		`Get-EventLog -LogName Application -Source '*zharp*' -Newest 50 | Format-List TimeGenerated, Message`)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// svcUninstall stops and deletes the service, then removes config.
func svcUninstall() error {
	exec.Command("sc.exe", "stop", winSvcName).Run() //nolint:errcheck
	waitForServiceState("STOPPED", 30*time.Second)
	exec.Command("sc.exe", "delete", winSvcName).Run() //nolint:errcheck
	os.RemoveAll(installConfigDir())                    //nolint:errcheck
	return nil
}

// svcRestart stops then starts the Windows service. Windows stops a service asynchronously, and a
// start sent while it's still stopping fails (1056: already running), so the stop is waited for.
func svcRestart() error {
	exec.Command("sc.exe", "stop", winSvcName).Run() //nolint:errcheck
	waitForServiceState("STOPPED", 30*time.Second)
	if out, err := exec.Command("sc.exe", "start", winSvcName).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForServiceState waits until sc.exe reports the service in state (RUNNING, STOPPED), up to
// timeout, and reports whether it got there.
func waitForServiceState(state string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		out, _ := exec.Command("sc.exe", "query", winSvcName).Output()
		if strings.Contains(string(out), state) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// printDoneCommands prints useful post-install commands for Windows.
func printDoneCommands() {
	uiDimMsg("Get-Service " + winSvcName)
	uiDimMsg("Restart-Service " + winSvcName)
	uiDimMsg(`notepad "` + installConfigFile() + `"`)
}
