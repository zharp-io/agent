package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestJournalExclusionList(t *testing.T) {
	re := regexp.MustCompile(journalExcludeRegex())
	for _, unit := range []string{
		"systemd-journald.service", "systemd-logind.service", "init.scope", "dbus.service",
		"cron.service", "CRON", "crond.service", "ssh.service", "sshd.service", "polkit.service",
		"NetworkManager.service", "NetworkManager-dispatcher.service", "snapd.service",
		"snapd.apparmor.service", "udisks2.service", "accounts-daemon.service", "rsyslog.service",
		"getty@tty1.service", "serial-getty@ttyS0.service", "user@1000.service", "session-3.scope",
		"kernel", "zharp-collector.service", "sudo",
	} {
		if !re.MatchString(unit) {
			t.Errorf("%s should be excluded", unit)
		}
	}
	for _, unit := range []string{"nginx.service", "myapp.service", "docker.service", "postgresql@16-main.service",
		"systemdish.service", "cronjob-runner.service", "sshuttle.service", "myapp"} {
		if re.MatchString(unit) {
			t.Errorf("%s should be kept", unit)
		}
	}
}

func TestPM2AppNames(t *testing.T) {
	for file, want := range map[string]string{
		"api-out.log":                      "api",
		"api-error.log":                    "api",
		"api-out-3.log":                    "api",
		"api-error-12.log":                 "api",
		"my-out-app-out.log":               "my-out-app",
		"worker-v2-error-0.log":            "worker-v2",
		"api-out__2026-10-19_00-00-00.log": "", // pm2-logrotate copy
		"pm2.log":                          "",
		"random.txt":                       "",
	} {
		if got := pm2AppName(file); got != want {
			t.Errorf("%s: got %q, want %q", file, got, want)
		}
	}
}

func TestPlanAutomaticCollectsAppLogs(t *testing.T) {
	granted := ""
	defer func(f func(string) error, d func() []pm2Dir) { grantPM2Access, pm2DirsFn = f, d }(grantPM2Access, pm2DirsFn)
	pm2DirsFn = func() []pm2Dir {
		return []pm2Dir{{Path: "/home/alice/.pm2/logs", Apps: []string{"api", "worker"}}, {Path: "/home/bob/.pm2/logs", Apps: []string{"web"}}}
	}
	grantPM2Access = func(dir string) error {
		if strings.Contains(dir, "bob") {
			return errors.New("setfacl: Operation not supported")
		}
		granted += dir
		return nil
	}

	detected := []ServiceOption{{Type: "docker", Detected: true}, {Type: "docker_logs", Detail: "3 containers"}, {Type: "journald"}, {Type: "pm2"}}
	plan := planAutomaticWith(detected, true)
	var ids []string
	for _, r := range plan.logReceivers {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "docker_logs,journald,filelog/pm2" {
		t.Errorf("log receivers: %v", ids)
	}
	if granted != "/home/alice/.pm2/logs" {
		t.Errorf("granted %q", granted)
	}
	inv := map[string]InventoryItem{}
	for _, it := range plan.inventory {
		inv[it.Type] = it
	}
	if inv["docker_logs"].Status != invCollecting || inv["docker_logs"].Label != "Docker container logs" || inv["docker_logs"].Detail != "3 containers" {
		t.Errorf("docker_logs: %+v", inv["docker_logs"])
	}
	if inv["journald"].Label != "Service logs (journal)" || inv["journald"].Status != invCollecting {
		t.Errorf("journald: %+v", inv["journald"])
	}
	if inv["pm2"].Status != invCollecting || inv["pm2"].Detail != "api, worker" {
		t.Errorf("pm2: %+v", inv["pm2"])
	}
	setup := inv["pm2:setup"]
	if setup.Status != invNeedsSetup || !strings.Contains(setup.Detail, "setfacl -R -m u:zharp-collector:rX /home/bob/.pm2/logs") ||
		!strings.Contains(setup.Detail, "sudo zharp-collector add pm2") {
		t.Errorf("pm2 needs_setup: %+v", setup)
	}
	if strings.Contains(plan.logReceivers[2].Block, "bob") {
		t.Error("a directory the collector can't read was configured")
	}

	off := planAutomaticWith(detected, false)
	if len(off.logReceivers) != 0 {
		t.Errorf("--no-app-logs still collects %v", off.logReceivers)
	}
	for _, it := range off.inventory {
		if isAppLogType(strings.Split(it.Type, ":")[0]) {
			t.Errorf("--no-app-logs still lists %s", it.Type)
		}
	}
}

func TestAppLogsDisabled(t *testing.T) {
	t.Setenv("ZHARP_NO_APP_LOGS", "")
	if appLogsDisabled(false) {
		t.Error("on by default")
	}
	if !appLogsDisabled(true) {
		t.Error("--no-app-logs")
	}
	t.Setenv("ZHARP_NO_APP_LOGS", "1")
	if !appLogsDisabled(false) {
		t.Error("ZHARP_NO_APP_LOGS=1")
	}
}

func TestJournalReplacesSyslogFiles(t *testing.T) {
	out := withoutJournalOverlap([]LogSource{
		{Name: "syslog", Paths: []string{"/var/log/syslog", "/var/log/auth.log"}},
		{Name: "messages", Paths: []string{"/var/log/messages"}},
		{Name: "nginx", Paths: []string{"/var/log/nginx/access.log"}},
	})
	// kern.log is only added when it exists; on the test machine it may not.
	if len(out) < 2 || out[0].Name != "syslog" || out[0].Paths[0] != "/var/log/auth.log" || contains(out[0].Paths, "/var/log/syslog") {
		t.Errorf("got %+v", out)
	}
	for _, s := range out {
		if s.Name == "messages" && !fileExists("/var/log/kern.log") {
			t.Errorf("an emptied source was kept: %+v", s)
		}
	}
}

func TestAddAppLogsToExistingConfig(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()

	content := buildConfigYAML([]string{"hostmetrics"}, nil,
		[]LogSource{{Name: "syslog", Paths: []string{"/var/log/syslog", "/var/log/messages"}}, {Name: "nginx", Paths: []string{"/var/log/nginx/access.log"}}},
		false, "")
	content = addLogReceiver(content, dockerLogsReceiver())
	content = addLogReceiver(content, pm2Receiver([]string{"/home/a/.pm2/logs"}))
	content = addLogReceiver(content, pm2Receiver([]string{"/home/a/.pm2/logs", "/root/.pm2/logs"})) // again: replaced, not duplicated
	content = addLogReceiver(content, journaldReceiver())
	content = dropJournalOverlap(content, false)

	if n := strings.Count(content, "\n  filelog/pm2:"); n != 1 {
		t.Errorf("filelog/pm2 appears %d times", n)
	}
	if !strings.Contains(content, "/root/.pm2/logs/*.log") {
		t.Error("second add pm2 didn't update the directories")
	}
	if strings.Contains(content, "/var/log/syslog") || strings.Contains(content, "filelog/syslog") {
		t.Errorf("syslog source should be gone once the journal is read:\n%s", content)
	}
	if !strings.Contains(content, "receivers: [filelog/nginx, docker_logs, filelog/pm2, journald]") {
		t.Errorf("logs pipeline:\n%s", content[strings.Index(content, "service:"):])
	}
	if !strings.Contains(content, "extensions: [health_check, file_storage]") || strings.Count(content, "\n  file_storage:\n") != 1 {
		t.Errorf("storage extension:\n%s", content)
	}
	validateContent(t, content, runtime.GOOS == "linux")
}

func TestGeneratedConfigValidates(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()
	pm2 := filepath.Join(t.TempDir(), ".pm2", "logs")
	receivers := []LogReceiver{dockerLogsReceiver(), pm2Receiver([]string{pm2})}
	if runtime.GOOS == "linux" {
		// journald only builds on Linux; elsewhere its factory refuses to create the receiver.
		receivers = append(receivers, journaldReceiver())
	}
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics", dockerReceiverIDs},
		receiverBlocks:   []string{dockerReceiverBlocks()},
		logSources:       []LogSource{{Name: "syslog", Paths: []string{"/var/log/auth.log"}}},
		logReceivers:     receivers,
		tracing:          true,
	})
	validateContent(t, content, true)
}

// validateContent writes content to a file and runs the collector's dry run over it.
func validateContent(t *testing.T, content string, withJournal bool) {
	t.Helper()
	if !withJournal {
		// Outside Linux the journald receiver can't be created; check the rest.
		content = removeFromLogsPipeline(removeReceiverBlock(content, "journald"), "journald")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	validateConfig(t, path)
}

func TestParsePM2List(t *testing.T) {
	// `pm2 jlist` nests the paths under pm2_env and may print a warning first.
	jlist := []byte("[PM2] Some warning\n" + `[{"name":"zharp-api","pm2_env":{"pm_out_log_path":"/srv/api/logs/out.log","pm_err_log_path":"/srv/api/logs/error.log"}}]`)
	// dump.pm2, what `pm2 save` writes, has them at the top.
	dump := []byte(`[{"name":"web","pm_out_log_path":"/home/a/.pm2/logs/web-out.log","pm_err_log_path":"/home/a/.pm2/logs/web-error.log"}]`)

	got := parsePM2List(jlist)
	if len(got) != 1 || got[0].Name != "zharp-api" || strings.Join(got[0].paths(), ",") != "/srv/api/logs/out.log,/srv/api/logs/error.log" {
		t.Errorf("jlist: %+v", got)
	}
	got = parsePM2List(dump)
	if len(got) != 1 || got[0].Name != "web" || got[0].paths()[0] != "/home/a/.pm2/logs/web-out.log" {
		t.Errorf("dump: %+v", got)
	}
	if parsePM2List([]byte("not json")) != nil {
		t.Error("garbage should give nil")
	}
}

func TestPM2ReceiverNamesAppsWithTheirOwnLogFiles(t *testing.T) {
	r := pm2ReceiverWith([]string{"/home/a/.pm2/logs"}, []pm2App{{Name: "zharp-api", Paths: []string{"/srv/api/logs/out.log", "/srv/api/logs/error.log"}}})
	for _, want := range []string{
		"'/home/a/.pm2/logs/*.log'",
		"'/srv/api/logs/out.log'",
		"'/srv/api/logs/error.log'",
		"include_file_path: true",
		`if: 'attributes["log.file.path"] in ["/srv/api/logs/out.log", "/srv/api/logs/error.log"]'`,
		"value: 'zharp-api'",
	} {
		if !strings.Contains(r.Block, want) {
			t.Errorf("block lacks %s:\n%s", want, r.Block)
		}
	}
	// Without such apps the receiver is what it always was.
	if r := pm2Receiver([]string{"/home/a/.pm2/logs"}); strings.Contains(r.Block, "log.file.path") || !strings.Contains(r.Block, "include_file_path: false") {
		t.Errorf("plain receiver changed:\n%s", r.Block)
	}
}

func TestPlanPM2GrantsAppLogFolders(t *testing.T) {
	defer func(f func(string) error) { grantPM2Access = f }(grantPM2Access)
	var granted []string
	grantPM2Access = func(dir string) error {
		if strings.Contains(dir, "locked") {
			return errors.New("setfacl: Operation not supported")
		}
		granted = append(granted, dir)
		return nil
	}
	o := planPM2(nil, []pm2App{
		{Name: "api", Paths: []string{"/srv/api/logs/out.log", "/srv/api/logs/error.log"}},
		{Name: "worker", Paths: []string{"/srv/locked/worker.log"}},
	})
	if len(o.apps) != 1 || o.apps[0].Name != "api" {
		t.Errorf("readable apps: %+v", o.apps)
	}
	if strings.Join(granted, ",") != "/srv/api/logs" {
		t.Errorf("granted once per folder: %v", granted)
	}
	if len(o.failed) != 1 || o.failed[0].Path != "/srv/locked" || !strings.Contains(o.commands, "locked") || !strings.Contains(o.commands, "add pm2") {
		t.Errorf("failed: %+v, commands %q", o.failed, o.commands)
	}
	inv := pm2Inventory(o)
	if inv[0].Status != invCollecting || inv[0].Detail != "api" || inv[1].Type != "pm2:setup" {
		t.Errorf("inventory: %+v", inv)
	}
}

func TestGeneratedConfigWithPM2AppFilesValidates(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()
	dir := t.TempDir()
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logReceivers: []LogReceiver{pm2ReceiverWith(
			[]string{filepath.Join(dir, ".pm2", "logs")},
			[]pm2App{{Name: "zharp-api", Paths: []string{filepath.Join(dir, "api", "out.log"), filepath.Join(dir, "api", "error.log")}}},
		)},
	})
	validateContent(t, content, false)
}
