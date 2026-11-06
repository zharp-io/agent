//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// askPM2 asks the PM2 that keeps its state in pm2Home for its apps (`pm2 jlist`), as the user it
// belongs to: run as root, PM2 would read root's own state, or start a second PM2. It is only
// asked while it runs (its socket is there), since asking a stopped PM2 starts one. nil when it
// can't be asked; the caller then reads what `pm2 save` wrote.
func askPM2(pm2Home string) []pm2Process {
	if !fileExists(filepath.Join(pm2Home, "rpc.sock")) {
		return nil
	}
	info, err := os.Stat(pm2Home)
	if err != nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	home := filepath.Dir(pm2Home)
	bin := pm2Binary(home)
	if bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "jlist")
	// pm2 is a node script: node is usually next to it (nvm, npm prefix), else on the usual PATH.
	cmd.Env = []string{
		"HOME=" + home,
		"PM2_HOME=" + pm2Home,
		"PATH=" + filepath.Dir(bin) + ":/usr/local/bin:/usr/bin:/bin",
	}
	if os.Geteuid() == 0 && st.Uid != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parsePM2List(out)
}

// pm2Binary finds pm2 for the user whose home this is: on the PATH, or where npm and nvm put it.
func pm2Binary(home string) string {
	candidates := []string{
		filepath.Join(home, ".npm-global", "bin", "pm2"),
		filepath.Join(home, ".local", "bin", "pm2"),
	}
	if nvm, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "pm2")); len(nvm) > 0 {
		sort.Sort(sort.Reverse(sort.StringSlice(nvm))) // the newest node version first
		candidates = append(candidates, nvm...)
	}
	if p, err := exec.LookPath("pm2"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/usr/local/bin/pm2", "/usr/bin/pm2")
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
