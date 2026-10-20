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
		metricsReceivers: []string{"hostmetrics", "docker_stats"},
		receiverBlocks:   []string{dockerBlock()},
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
