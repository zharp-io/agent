package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Application logs the collector finds by itself: Docker container output (through the Engine
// API), systemd services (through the journal) and PM2 apps (their log files). They are
// collected by default; `install --no-app-logs` or ZHARP_NO_APP_LOGS=1 leaves them out.

const (
	agentUser = "zharp-collector"

	// storageExtension remembers the journal cursor and PM2 file offsets across restarts.
	storageExtension = "file_storage"
)

// journalExcludedUnits are the systemd units (and, for entries without a unit, syslog
// identifiers) that are part of the operating system rather than an application. Their entries
// are dropped from the journal source; everything else on the journal is an application
// service and reported under its unit name. Patterns are matched case-insensitively against the
// unit with or without its type suffix (.service, .scope, .socket, .timer, .mount, .slice);
// "*" matches anything. This is the one list — the docs quote it.
var journalExcludedUnits = []string{
	"systemd", "systemd-*", "init", // PID 1 (init.scope) and its helpers
	"dbus", "dbus-broker",
	"cron", "crond", "anacron",
	"ssh", "sshd",
	"polkit", "polkitd",
	"NetworkManager", "NetworkManager-*",
	"snapd", "snapd.*",
	"udisks2",
	"accounts-daemon",
	"rsyslog",
	"getty@*", "serial-getty@*",
	"user@*", "session-*",
	"sudo", "su", "login",
	"kernel",
	agentUser, // the collector itself
}

// journalExcludeRegex turns journalExcludedUnits into one anchored regular expression.
func journalExcludeRegex() string {
	parts := make([]string, 0, len(journalExcludedUnits))
	for _, u := range journalExcludedUnits {
		parts = append(parts, strings.ReplaceAll(regexp.QuoteMeta(u), `\*`, `.*`))
	}
	return `(?i)^(` + strings.Join(parts, "|") + `)(\.(service|scope|socket|timer|mount|slice))?$`
}

// installStateDir holds what the collector remembers between runs (journal cursor, file
// offsets, Docker log positions). It must be writable by the collector's user.
func installStateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(installConfigDir(), "state")
	}
	return "/var/lib/zharp-collector"
}

// stateDirOverride lets tests point the generated config at a temporary directory.
var stateDirOverride string

func stateDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	return installStateDir()
}

// LogReceiver is a logs-pipeline receiver other than a plain file tail.
type LogReceiver struct {
	ID      string // receiver id in the config, e.g. docker_logs, journald, filelog/pm2
	Block   string // its YAML under receivers:, indented by two spaces
	Storage bool   // it uses the file_storage extension
}

func storageExtensionBlock() string {
	return fmt.Sprintf("  %s:\n    directory: %s\n    create_directory: true\n",
		storageExtension, yamlQuote(filepath.Join(stateDir(), "storage")))
}

func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func dockerEndpoint() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

// dockerLogsReceiver reads every container's output through the Docker API. The state file is
// where it remembers the last line per container, so a restart neither replays nor skips.
func dockerLogsReceiver() LogReceiver {
	return LogReceiver{ID: "docker_logs", Block: fmt.Sprintf(
		"  docker_logs:\n    endpoint: %s\n    state_file: %s\n",
		dockerEndpoint(), yamlQuote(filepath.Join(stateDir(), "docker_logs.json")))}
}

// journaldReceiver reads application services from the systemd journal.
//
// Each entry becomes one line: the body is MESSAGE, service.name is the unit without
// ".service" (or the syslog identifier when there is no unit), and the level comes from
// PRIORITY — 0-2 fatal, 3 error, 4 warning, 5-6 info, 7 debug. Kernel and audit records, the
// units in journalExcludedUnits, and containers using Docker's journald log driver (read by
// docker_logs already) are dropped. It starts at the end of the journal and keeps its cursor in
// file_storage, so a restart picks up exactly where it stopped.
func journaldReceiver() LogReceiver {
	exclude := strings.ReplaceAll(journalExcludeRegex(), `\`, `\\`)
	unitOrIdent := `(body._SYSTEMD_UNIT ?? body.SYSLOG_IDENTIFIER ?? "")`
	var sb strings.Builder
	sb.WriteString("  journald:\n")
	sb.WriteString("    start_at: end\n")
	sb.WriteString("    priority: debug\n")
	sb.WriteString("    convert_message_bytes: true\n")
	fmt.Fprintf(&sb, "    storage: %s\n", storageExtension)
	sb.WriteString("    operators:\n")
	sb.WriteString("      # Operating-system units are not application logs: journalExcludedUnits in cli_applogs.go.\n")
	sb.WriteString("      - type: filter\n")
	fmt.Fprintf(&sb, "        expr: '(body._TRANSPORT ?? \"\") in [\"kernel\", \"audit\"] or body.CONTAINER_ID != nil or %s matches \"%s\"'\n", unitOrIdent, exclude)
	sb.WriteString("      - type: severity_parser\n")
	sb.WriteString("        parse_from: body.PRIORITY\n")
	sb.WriteString("        overwrite_text: true\n")
	sb.WriteString("        mapping:\n")
	sb.WriteString("          fatal: ['0', '1', '2']\n")
	sb.WriteString("          error: '3'\n")
	sb.WriteString("          warn: '4'\n")
	sb.WriteString("          info: ['5', '6']\n")
	sb.WriteString("          debug: '7'\n")
	sb.WriteString("        on_error: send_quiet\n")
	sb.WriteString("      - type: add\n")
	sb.WriteString("        field: resource[\"service.name\"]\n")
	sb.WriteString("        value: 'EXPR(body._SYSTEMD_UNIT != nil ? trimSuffix(body._SYSTEMD_UNIT, \".service\") : (body.SYSLOG_IDENTIFIER ?? \"journal\"))'\n")
	sb.WriteString("      - type: copy\n")
	sb.WriteString("        from: body._SYSTEMD_UNIT\n")
	sb.WriteString("        to: attributes[\"systemd.unit\"]\n")
	sb.WriteString("        on_error: send_quiet\n")
	sb.WriteString("      - type: move\n")
	sb.WriteString("        from: body.MESSAGE\n")
	sb.WriteString("        to: body\n")
	sb.WriteString("        on_error: send_quiet\n")
	sb.WriteString(recombineOperator(`attributes["systemd.unit"]`))
	return LogReceiver{ID: "journald", Block: sb.String(), Storage: true}
}

// pm2FileRe splits a PM2 log file name: <app>-out.log, <app>-error.log, and in cluster mode
// <app>-out-<n>.log. pm2-logrotate's rotated copies (<app>-out__<date>.log) are excluded.
var pm2FileRe = regexp.MustCompile(`^(.+?)-(out|error|err)(?:-(\d+))?\.log$`)

// pm2Receiver tails the PM2 log directories; the app name, taken from each file's name, is the
// service name. File offsets are kept in file_storage so a restart doesn't skip or repeat.
func pm2Receiver(dirs []string) LogReceiver {
	var sb strings.Builder
	sb.WriteString("  filelog/pm2:\n")
	sb.WriteString("    include:\n")
	for _, d := range dirs {
		fmt.Fprintf(&sb, "      - %s\n", yamlQuote(filepath.ToSlash(filepath.Join(d, "*.log"))))
	}
	sb.WriteString("    exclude:\n")
	for _, d := range dirs {
		fmt.Fprintf(&sb, "      - %s\n", yamlQuote(filepath.ToSlash(filepath.Join(d, "*__*.log"))))
	}
	sb.WriteString("    include_file_name: true\n")
	sb.WriteString("    include_file_path: false\n")
	fmt.Fprintf(&sb, "    storage: %s\n", storageExtension)
	sb.WriteString("    preserve_leading_whitespaces: true\n")
	sb.WriteString("    operators:\n")
	sb.WriteString("      - type: add\n")
	sb.WriteString("        field: resource[\"service.name\"]\n")
	sb.WriteString("        value: pm2\n")
	sb.WriteString("      - type: regex_parser\n")
	sb.WriteString("        parse_from: attributes[\"log.file.name\"]\n")
	sb.WriteString("        regex: '^(?P<pm2_app>.+?)-(?P<pm2_stream>out|error|err)(?:-(?P<pm2_instance>\\d+))?\\.log$'\n")
	sb.WriteString("        on_error: send_quiet\n")
	sb.WriteString("      - type: move\n")
	sb.WriteString("        from: attributes.pm2_app\n")
	sb.WriteString("        to: resource[\"service.name\"]\n")
	sb.WriteString("        on_error: send_quiet\n")
	sb.WriteString(recombineOperator(`attributes["log.file.name"]`))
	sb.WriteString(timestampOperators())
	return LogReceiver{ID: "filelog/pm2", Block: sb.String(), Storage: true}
}

// pm2AppName is the service name a PM2 log file maps to ("" when it isn't a PM2 log).
func pm2AppName(file string) string {
	m := pm2FileRe.FindStringSubmatch(filepath.Base(file))
	if m == nil || strings.Contains(file, "__") {
		return ""
	}
	return m[1]
}

// journalAvailable reports whether this host runs systemd with a journal to read.
func journalAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return fileExists("/run/systemd/system") && hasBin("journalctl")
}

// appLogsDisabled is --no-app-logs or ZHARP_NO_APP_LOGS set to a true value.
func appLogsDisabled(flagSet bool) bool {
	if flagSet {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZHARP_NO_APP_LOGS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// pm2DirsFn finds the PM2 log directories; tests replace it.
var pm2DirsFn = findPM2Dirs

// pm2Dir is one PM2 log directory and the apps logging into it.
type pm2Dir struct {
	Path string
	Apps []string
}

// homeDirs lists the home directories of root and every user with one: /etc/passwd first, then
// /root and /home/* in case accounts come from LDAP or similar.
func homeDirs() []string {
	seen := map[string]bool{}
	var homes []string
	add := func(h string) {
		h = filepath.Clean(h)
		if h == "/" || h == "." || seen[h] {
			return
		}
		seen[h] = true
		homes = append(homes, h)
	}
	add("/root")
	if f, err := os.Open("/etc/passwd"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Split(sc.Text(), ":")
			if len(fields) >= 6 && fields[5] != "" && !strings.HasPrefix(fields[5], "/nonexistent") {
				add(fields[5])
			}
		}
		f.Close()
	}
	if matches, _ := filepath.Glob("/home/*"); matches != nil {
		for _, m := range matches {
			add(m)
		}
	}
	return homes
}

// findPM2Dirs returns the PM2 log directories on this host that hold at least one app log:
// <home>/.pm2/logs for root and every user, and $PM2_HOME/logs of whoever runs the install.
func findPM2Dirs() []pm2Dir {
	if runtime.GOOS != "linux" {
		return nil
	}
	var candidates []string
	if h := strings.TrimSpace(os.Getenv("PM2_HOME")); h != "" {
		candidates = append(candidates, filepath.Join(h, "logs"))
	}
	for _, home := range homeDirs() {
		candidates = append(candidates, filepath.Join(home, ".pm2", "logs"))
	}
	seen := map[string]bool{}
	var out []pm2Dir
	for _, dir := range candidates {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		apps := map[string]bool{}
		for _, e := range entries {
			if name := pm2AppName(e.Name()); name != "" && !e.IsDir() {
				apps[name] = true
			}
		}
		if len(apps) == 0 {
			continue
		}
		d := pm2Dir{Path: dir}
		for a := range apps {
			d.Apps = append(d.Apps, a)
		}
		sort.Strings(d.Apps)
		out = append(out, d)
	}
	return out
}

func pm2Apps(dirs []pm2Dir) []string {
	seen := map[string]bool{}
	var apps []string
	for _, d := range dirs {
		for _, a := range d.Apps {
			if !seen[a] {
				seen[a] = true
				apps = append(apps, a)
			}
		}
	}
	sort.Strings(apps)
	return apps
}

// pm2GrantCommands are the commands that let the collector's user read one PM2 log directory
// and nothing else: traverse-only (x) on each parent that doesn't already allow it, read on
// the directory and its files, and a default ACL so files PM2 creates later are readable too.
// Home directories are never chmod-ed.
func pm2GrantCommands(dir string) [][]string {
	var cmds [][]string
	for _, p := range parentsNeedingTraverse(dir) {
		cmds = append(cmds, []string{"setfacl", "-m", "u:" + agentUser + ":x", p})
	}
	cmds = append(cmds,
		[]string{"setfacl", "-R", "-m", "u:" + agentUser + ":rX", dir},
		[]string{"setfacl", "-d", "-m", "u:" + agentUser + ":rX", dir},
	)
	return cmds
}

// parentsNeedingTraverse lists the directories above dir (outermost first) that others can't
// enter, so the collector's user needs an x entry to reach dir.
func parentsNeedingTraverse(dir string) []string {
	var parents []string
	for p := filepath.Dir(filepath.Clean(dir)); p != "/" && p != "." && p != filepath.Dir(p); p = filepath.Dir(p) {
		parents = append([]string{p}, parents...)
	}
	var out []string
	for _, p := range parents {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o001 != 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

func shellJoin(cmds [][]string) string {
	var lines []string
	for _, c := range cmds {
		quoted := make([]string, len(c))
		for i, a := range c {
			if strings.ContainsAny(a, " '\"$`\\") {
				quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
			} else {
				quoted[i] = a
			}
		}
		lines = append(lines, "sudo "+strings.Join(quoted, " "))
	}
	return strings.Join(lines, " && ")
}

// pm2Outcome is what planPM2 managed. Directories that couldn't be opened up are left out of the
// config and reported as waiting, with the exact commands to run, ending in
// `zharp-collector add pm2`.
type pm2Outcome struct {
	granted  []pm2Dir
	failed   []pm2Dir
	commands string // what to run by hand for the failed ones, joined with &&
	noACL    bool   // setfacl isn't installed
}

func planPM2(dirs []pm2Dir) pm2Outcome {
	var o pm2Outcome
	var manual [][]string
	for _, d := range dirs {
		if err := grantPM2Access(d.Path); err != nil {
			o.failed = append(o.failed, d)
			manual = append(manual, pm2GrantCommands(d.Path)...)
			continue
		}
		o.granted = append(o.granted, d)
	}
	if len(manual) > 0 {
		o.noACL = !hasBin("setfacl")
		o.commands = shellJoin(append(manual, []string{"zharp-collector", "add", "pm2"}))
	}
	return o
}

// pm2Inventory describes the outcome for the dashboard. A waiting item's detail is
// "<why>. Run: <commands>"; the dashboard shows the commands as a step to copy.
func pm2Inventory(o pm2Outcome) []InventoryItem {
	var items []InventoryItem
	if len(o.granted) > 0 {
		items = append(items, InventoryItem{Type: "pm2", Label: "PM2 app logs", Status: invCollecting, Detail: strings.Join(pm2Apps(o.granted), ", ")})
	}
	if len(o.failed) > 0 {
		var paths []string
		for _, d := range o.failed {
			paths = append(paths, d.Path)
		}
		typ := "pm2"
		if len(o.granted) > 0 {
			typ = "pm2:setup"
		}
		why := fmt.Sprintf("The collector can't read %s yet", strings.Join(paths, ", "))
		if o.noACL {
			why += " (install the acl package first: sudo apt-get install acl, or sudo dnf install acl)"
		}
		items = append(items, InventoryItem{Type: typ, Label: "PM2 app logs", Status: invNeedsSetup,
			Detail: why + ". Run: " + o.commands})
	}
	return items
}

func dirPaths(dirs []pm2Dir) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}

// Syslog and the journal carry the same lines on most distributions (rsyslog reads them from
// journald). When the journal source is on, /var/log/syslog and /var/log/messages are dropped:
// the application services come from the journal under their own names, and what the journal
// source leaves out on purpose — login (auth.log) and kernel (kern.log) lines — keeps coming
// from those files. General OS chatter (systemd, cron, dbus...) is not collected twice or at all.
var syslogFilesCoveredByJournal = map[string]bool{"/var/log/syslog": true, "/var/log/messages": true}

// syslogPaths are the system log files to tail, given whether the journal is read.
func syslogPaths(journal bool) []string {
	if journal {
		return existing("/var/log/auth.log", "/var/log/kern.log")
	}
	return existing("/var/log/syslog", "/var/log/messages", "/var/log/auth.log")
}

// withoutJournalOverlap removes the syslog files the journal already covers from the sources,
// adding kern.log in their place, and drops sources left with nothing to tail.
func withoutJournalOverlap(sources []LogSource) []LogSource {
	var out []LogSource
	for _, s := range sources {
		var paths []string
		dropped := false
		for _, p := range s.Paths {
			if syslogFilesCoveredByJournal[p] {
				dropped = true
				continue
			}
			paths = append(paths, p)
		}
		if dropped {
			for _, p := range existing("/var/log/kern.log") {
				if !contains(paths, p) {
					paths = append(paths, p)
				}
			}
		}
		if len(paths) > 0 {
			s.Paths = paths
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// isAppLogType is true for the application-log sources --no-app-logs turns off.
func isAppLogType(t string) bool {
	return t == "docker_logs" || t == "journald" || t == "pm2"
}

// addSystemLogs tails the system log files, minus what the journal source already covers.
func (p *autoPlan) addSystemLogs(journal bool) {
	paths := syslogPaths(journal)
	if journal && len(paths) == 0 {
		return // nothing the journal doesn't already carry
	}
	p.addLogs("syslog", "System logs", "syslog", paths)
}

// withoutAppLogs drops the application-log sources (for --no-app-logs).
func withoutAppLogs(opts []ServiceOption) []ServiceOption {
	var out []ServiceOption
	for _, o := range opts {
		if !isAppLogType(o.Type) {
			out = append(out, o)
		}
	}
	return out
}

// configureAppLogs sets up one application-log source for the wizard and for `add`. ok is false
// when there is nothing to collect yet; items still say why, for the dashboard.
func configureAppLogs(svcType string) (r LogReceiver, items []InventoryItem, ok bool) {
	switch svcType {
	case "docker_logs":
		cnt, _ := dockerInfo()
		uiOK("Docker container logs configured (read through the Docker API).")
		uiDimMsg("Opt a container out with the label zharp.logs=false.")
		return dockerLogsReceiver(), []InventoryItem{{Type: svcType, Label: "Docker container logs", Status: invCollecting, Detail: dockerLogsOption(cnt).Detail}}, true
	case "journald":
		if !journalAvailable() {
			uiWarn("No systemd journal on this host (journalctl and /run/systemd/system are needed).")
			return LogReceiver{}, nil, false
		}
		uiOK("Service logs configured: every systemd service, by unit name.")
		uiDimMsg("Operating-system units (systemd-*, sshd, cron, dbus, ...) are left out.")
		return journaldReceiver(), []InventoryItem{{Type: svcType, Label: "Service logs (journal)", Status: invCollecting, Detail: "systemd services; operating-system units left out"}}, true
	case "pm2":
		dirs := pm2DirsFn()
		if len(dirs) == 0 {
			uiWarn("No PM2 logs found (looked in ~/.pm2/logs of root and every user, and $PM2_HOME/logs).")
			return LogReceiver{}, nil, false
		}
		o := planPM2(dirs)
		for _, d := range o.granted {
			uiOK(fmt.Sprintf("PM2 logs in %s readable by %s: %s", d.Path, agentUser, strings.Join(d.Apps, ", ")))
		}
		if len(o.failed) > 0 {
			uiWarn("Could not give the collector read access to some PM2 logs. Run:")
			if o.noACL {
				uiDimMsg("setfacl is missing: install the acl package first (sudo apt-get install acl, or sudo dnf install acl).")
			}
			uiDimMsg(o.commands)
		}
		items = pm2Inventory(o)
		if len(o.granted) == 0 {
			return LogReceiver{}, items, false
		}
		return pm2Receiver(dirPaths(o.granted)), items, true
	}
	return LogReceiver{}, nil, false
}
