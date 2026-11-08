package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

// inspectHelperDir holds the read-only helper.
const inspectHelperDir = "/usr/local/lib/zharp-collector"

// ensureInspectHelper puts the read-only helper next to the collector: a copy of binary, owned by
// root, runnable only by the zharp-collector group, with CAP_DAC_READ_SEARCH alone — enough to
// list every folder and see file sizes, nothing else: it can't write, can't read what files say
// beyond their names, and refuses to do anything but measure disk usage (runInspectHelper).
// Without setcap (libcap2-bin) there's no helper, and the scan counts what the agent's user can
// read. Returns a line for the install or update summary.
func ensureInspectHelper(binary string) string {
	if os.Getuid() != 0 {
		return ""
	}
	setcap, err := exec.LookPath("setcap")
	if err != nil {
		_ = os.Remove(filepath.Join(inspectHelperDir, inspectHelperName))
		return "Disk usage: only folders the agent can read are measured (install libcap2-bin for setcap, then run update)"
	}
	if err := os.MkdirAll(inspectHelperDir, 0o755); err != nil {
		return "Disk usage: " + err.Error()
	}
	target := filepath.Join(inspectHelperDir, inspectHelperName)
	tmp := target + ".new"
	if err := copyFile(binary, tmp); err != nil {
		return "Disk usage: " + err.Error()
	}
	gid := 0
	if g, err := user.LookupGroup("zharp-collector"); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	}
	if err := os.Chown(tmp, 0, gid); err != nil {
		_ = os.Remove(tmp)
		return "Disk usage: " + err.Error()
	}
	if err := os.Chmod(tmp, 0o750); err != nil {
		_ = os.Remove(tmp)
		return "Disk usage: " + err.Error()
	}
	if out, err := exec.Command(setcap, "cap_dac_read_search+ep", tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Sprintf("Disk usage: setcap failed (%s): only folders the agent can read are measured", string(out))
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "Disk usage: " + err.Error()
	}
	return ""
}

// removeInspectHelper is uninstall's part.
func removeInspectHelper() {
	_ = os.RemoveAll(inspectHelperDir)
}
