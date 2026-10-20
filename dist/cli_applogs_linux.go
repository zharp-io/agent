//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// ensureAgentUser creates the unprivileged user the service runs as, if it isn't there yet.
// It is needed before the service exists, to hand out read access (PM2 log ACLs).
func ensureAgentUser() {
	if _, err := user.Lookup(agentUser); err == nil {
		return
	}
	_ = exec.Command("useradd", "--system", "--no-create-home", "--shell", "/sbin/nologin", agentUser).Run()
}

// grantPM2AccessOS opens one PM2 log directory to the collector's user with ACLs.
func grantPM2AccessOS(dir string) error {
	ensureAgentUser()
	if agentCanRead(dir) {
		return nil // already readable: world-readable path, or granted by an earlier run
	}
	if !hasBin("setfacl") {
		return errors.New("setfacl is not installed")
	}
	ensureAgentUser()
	for _, c := range pm2GrantCommands(dir) {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

var grantPM2Access = grantPM2AccessOS

// agentCanRead checks, as the collector's user, that dir can be listed and its log files read.
func agentCanRead(dir string) bool {
	if !hasBin("runuser") {
		return false
	}
	script := `cd "$1" && ls >/dev/null && for f in *.log; do [ -e "$f" ] || continue; [ -r "$f" ] || exit 1; done`
	return exec.Command("runuser", "-u", agentUser, "--", "sh", "-c", script, "sh", dir).Run() == nil
}

// prepareStateDir creates the directory the collector keeps its positions in, owned by its user.
func prepareStateDir() error {
	dir := installStateDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	ensureAgentUser()
	u, err := user.Lookup(agentUser)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return chownTree(dir, uid, gid)
}

func chownTree(dir string, uid, gid int) error {
	return exec.Command("chown", "-R", fmt.Sprintf("%d:%d", uid, gid), dir).Run()
}

// grantDockerAccess lets the collector's user talk to the Docker socket.
func grantDockerAccess() {
	ensureAgentUser()
	_ = exec.Command("usermod", "-aG", "docker", agentUser).Run()
}
