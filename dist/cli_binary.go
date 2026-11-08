package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// installedBinaryPath is where install puts the agent on Linux and macOS, as install.sh does.
const installedBinaryPath = "/usr/local/bin/zharp-collector"

// serviceBinary is the binary the service runs. On Linux and macOS the agent is copied to
// /usr/local/bin first when it's run from anywhere else: the service runs as its own user
// (zharp-collector on Linux), which can't enter a home folder, so a binary downloaded to
// ~/zharp-collector and installed from there would fail to start (permission denied). Windows
// services run as SYSTEM and can read it where it is.
func serviceBinary(current string) (string, error) {
	if runtime.GOOS == "windows" {
		return current, nil
	}
	if resolved, err := filepath.EvalSymlinks(current); err == nil && resolved == installedBinaryPath {
		return installedBinaryPath, nil
	}
	if current == installedBinaryPath {
		return current, nil
	}
	if err := os.MkdirAll(filepath.Dir(installedBinaryPath), 0o755); err != nil {
		return "", err
	}
	tmp := installedBinaryPath + ".new"
	if err := copyFile(current, tmp); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, installedBinaryPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return installedBinaryPath, nil
}
