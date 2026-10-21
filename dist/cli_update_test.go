package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zharp-io/agent/logrules"
)

// blockText is "  <id>:" and everything indented under it, in the section starting at section.
func blockText(t *testing.T, content, section, id string) string {
	t.Helper()
	s := strings.Index(content, "\n"+section+":\n")
	if s < 0 {
		t.Fatalf("no %s section", section)
	}
	header := "\n  " + id + ":\n"
	start := strings.Index(content[s:], header)
	if start < 0 {
		t.Fatalf("no %s in %s", id, section)
	}
	start += s + 1
	end := start + len(header) - 1
	for end < len(content) {
		nl := strings.IndexByte(content[end:], '\n')
		if nl < 0 {
			return content[start:]
		}
		line := content[end : end+nl]
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "   ") {
			break
		}
		end += nl + 1
	}
	return strings.TrimRight(content[start:end], "\n") + "\n" // blank lines after it aren't its own
}

// validateMerged runs the collector's dry run over a merged config. The journal receiver only
// starts on Linux, so elsewhere it is taken out first.
func validateMerged(t *testing.T, content string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		content = removeFromLogsPipeline(removeReceiverBlock(content, "journald"), "journald")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	validateConfig(t, path)
}

func section(content, name, next string) string {
	s := strings.Index(content, "\n"+name+":\n")
	e := strings.Index(content, "\n"+next+":\n")
	if s < 0 || e < s {
		return ""
	}
	return content[s:e]
}

func receiverIDList(t *testing.T, content string) []string {
	t.Helper()
	rs, err := configReceivers(content)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

// fakeHost stands in for detection and the host changes an update makes.
func fakeHost(t *testing.T) {
	t.Helper()
	stateDirOverride = t.TempDir()
	saved := []any{grantPM2Access, pm2DirsFn, detectFn, validateConfigFn, restartFn, grantDockerFn, prepareStateFn, installRules}
	t.Cleanup(func() {
		stateDirOverride = ""
		grantPM2Access = saved[0].(func(string) error)
		pm2DirsFn = saved[1].(func() []pm2Dir)
		detectFn = saved[2].(func() []ServiceOption)
		validateConfigFn = saved[3].(func(string, string) error)
		restartFn = saved[4].(func() error)
		grantDockerFn = saved[5].(func())
		prepareStateFn = saved[6].(func() error)
		installRules = saved[7].(logRulesSpec)
	})
	grantPM2Access = func(string) error { return nil }
	pm2DirsFn = func() []pm2Dir {
		return []pm2Dir{{Path: "/home/deploy/.pm2/logs", Apps: []string{"api", "worker"}}}
	}
	validateConfigFn = func(string, string) error { return nil }
	restartFn = func() error { return nil }
	grantDockerFn = func() {}
	prepareStateFn = func() error { return nil }
}

const pgBlock = `
  postgresql:
    endpoint: db.internal:5432
    username: zharp_monitor
    password: "${env:PG_PASSWORD}"
    databases:
      - shop
    collection_interval: 30s
    tls:
      insecure: true
`

// installedConfig is a host set up before app logs: Postgres added by hand, a custom log file,
// an exclude rule and a custom endpoint.
func installedConfig(t *testing.T) string {
	t.Helper()
	installRules = logRulesSpec{Rules: []logrules.Rule{{Type: logrules.RuleExclude, Pattern: "GET /healthz"}}}
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logSources:       []LogSource{{Name: "myapp", Paths: []string{"/srv/myapp/logs/*.log"}}},
		tracing:          true,
		endpoint:         "https://zharp.example.com",
	})
	installRules = logRulesSpec{}
	content, err := addReceiverToContent(content, "postgresql", pgBlock)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

var installedInventory = []InventoryItem{
	{Type: "host", Label: "Host metrics", Status: invCollecting, Detail: "CPU, memory, disk, network and load"},
	{Type: "apm", Label: "Application traces", Status: invCollecting},
	{Type: "pg", Label: "PostgreSQL", Status: invCollecting},
	{Type: "log:myapp", Label: "myapp logs", Status: invCollecting, Detail: "/srv/myapp/logs/*.log"},
}

var newerDetections = []ServiceOption{
	{Type: "pg", Detected: true},
	{Type: "docker", Detected: true},
	{Type: "docker_logs", Detail: "3 containers", Detected: true},
	{Type: "journald", Detected: true},
	{Type: "pm2", Detected: true},
}

func TestMergeAddsOnlyWhatIsMissing(t *testing.T) {
	fakeHost(t)
	before := installedConfig(t)

	res, err := mergeSources(before, installedInventory, newerDetections, true)
	if err != nil {
		t.Fatal(err)
	}
	after := res.config

	if got := strings.Join(receiverIDList(t, after), ","); got != "hostmetrics,filelog/myapp,otlp,postgresql,docker_stats,docker_logs,journald,filelog/pm2" {
		t.Errorf("receivers: %s", got)
	}
	if strings.Count(after, "\n  postgresql:") != 1 {
		t.Error("postgresql added twice")
	}

	// What was configured is still there, byte for byte.
	for _, id := range []string{"hostmetrics", "filelog/myapp", "otlp", "postgresql"} {
		if a, b := blockText(t, before, "receivers", id), blockText(t, after, "receivers", id); a != b {
			t.Errorf("receiver %s changed:\n--- before\n%s--- after\n%s", id, a, b)
		}
	}
	for _, sec := range [][2]string{{"processors", "exporters"}, {"exporters", "service"}} {
		if a, b := section(before, sec[0], sec[1]), section(after, sec[0], sec[1]); a == "" || a != b {
			t.Errorf("%s changed:\n--- before\n%s\n--- after\n%s", sec[0], a, b)
		}
	}
	if !strings.Contains(after, `pattern: 'GET /healthz'`) || !strings.Contains(after, `endpoint: "https://zharp.example.com"`) {
		t.Error("rules or endpoint lost")
	}
	// The pipelines only gained receivers; their processors are as they were.
	for _, want := range []string{
		"      receivers: [hostmetrics, postgresql, docker_stats]\n      processors: [memory_limiter, resourcedetection, batch]\n",
		"      receivers: [filelog/myapp, docker_logs, journald, filelog/pm2]\n      processors: [memory_limiter, zharp_logs, resourcedetection, batch]\n",
		"  extensions: [health_check, file_storage]\n",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("missing %q in\n%s", want, after)
		}
	}
	if strings.Count(after, "    traces:\n") != 1 {
		t.Error("traces pipeline duplicated")
	}

	// The real collector accepts it.
	validateMerged(t, after)

	inv := map[string]InventoryItem{}
	for _, it := range res.inventory {
		inv[it.Type] = it
	}
	if !reflect.DeepEqual(inv["pg"], installedInventory[2]) || !reflect.DeepEqual(inv["log:myapp"], installedInventory[3]) {
		t.Errorf("existing inventory entries changed: %+v %+v", inv["pg"], inv["log:myapp"])
	}
	for _, typ := range []string{"docker", "docker_logs", "journald", "pm2"} {
		if inv[typ].Status != invCollecting {
			t.Errorf("%s: %+v", typ, inv[typ])
		}
	}
	var added []string
	for _, it := range res.added {
		added = append(added, it.Type)
	}
	if strings.Join(added, ",") != "docker,docker_logs,journald,pm2" {
		t.Errorf("added: %v", added)
	}
	if addedDetail(inv["pm2"]) != "2 apps: api, worker" {
		t.Errorf("pm2 summary: %q", addedDetail(inv["pm2"]))
	}
	if !res.docker || !res.stateDir {
		t.Errorf("docker=%v stateDir=%v, want both", res.docker, res.stateDir)
	}

	// A second run finds nothing to add.
	again, err := mergeSources(after, res.inventory, newerDetections, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.config != after || len(again.added) != 0 || !reflect.DeepEqual(again.inventory, res.inventory) {
		t.Errorf("second run changed something: added %v\n%s", again.added, again.config)
	}
}

func TestMergeLeavesUnconfiguredDatabaseForSetup(t *testing.T) {
	fakeHost(t)
	before := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true})
	mr, mb := mysqlReceivers("localhost", "3306", "zharp_monitor")
	before, _ = addReceiverToContent(before, mr, mb)
	inventory := []InventoryItem{{Type: "mysql", Label: "MySQL / MariaDB", Status: invCollecting, Detail: "set up by hand"}}

	res, err := mergeSources(before, inventory, []ServiceOption{{Type: "pg"}, {Type: "mysql"}, {Type: "mongo"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != before {
		t.Errorf("config changed for databases that need a password:\n%s", res.config)
	}
	inv := map[string]InventoryItem{}
	for _, it := range res.inventory {
		inv[it.Type] = it
	}
	if inv["pg"].Status != invNeedsSetup || inv["mongo"].Status != invNeedsSetup {
		t.Errorf("pg/mongo should need setup: %+v %+v", inv["pg"], inv["mongo"])
	}
	if inv["mysql"] != inventory[0] {
		t.Errorf("collecting mysql entry changed: %+v", inv["mysql"])
	}
	if _, how := setupHint(inv["pg"]); !strings.Contains(how, "zharp-collector add postgres --user zharp_monitor --password PASSWORD") {
		t.Errorf("pg hint: %q", how)
	}
}

func TestMergeRespectsNoAppLogsAndSkippedSources(t *testing.T) {
	fakeHost(t)
	before := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true})
	inventory := []InventoryItem{{Type: "docker", Label: "Docker", Status: invNeedsSetup, Detail: skippedDuringInstall}}

	res, err := mergeSources(before, inventory, newerDetections, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != before {
		t.Errorf("added with app logs off and Docker skipped:\n%s", res.config)
	}
	if len(res.notAdded) != 1 || !strings.Contains(res.notAdded[0], "skipped during install") {
		t.Errorf("not added: %v", res.notAdded)
	}

	// Installed with --no-app-logs: remembered in the config.
	marked := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true, noAppLogs: true})
	dir := t.TempDir()
	p := updatePaths{config: filepath.Join(dir, "config.yaml"), env: filepath.Join(dir, "env"), inventory: filepath.Join(dir, "inventory.json")}
	_ = os.WriteFile(p.config, []byte(marked), 0o644)
	detectFn = func() []ServiceOption { return newerDetections[3:] } // journald, pm2
	out, err := applyUpdate(p, applyOptions{appLogs: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.configChanged || len(out.res.added) != 0 {
		t.Errorf("app logs added to a --no-app-logs install: %v", out.res.added)
	}
}

func TestMergeKeepsSyslogRatherThanAddJournal(t *testing.T) {
	fakeHost(t)
	before := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true,
		logSources: []LogSource{{Name: "syslog", Paths: []string{"/var/log/syslog", "/var/log/auth.log"}}}})
	res, err := mergeSources(before, nil, []ServiceOption{{Type: "journald"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != before || len(res.notAdded) != 1 || !strings.Contains(res.notAdded[0], "add journal") {
		t.Errorf("journal: notAdded %v\n%s", res.notAdded, res.config)
	}
}

func TestMergeAddsTracingAndLogsPipelineToOldConfig(t *testing.T) {
	fakeHost(t)
	before := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}})
	res, err := mergeSources(before, nil, []ServiceOption{{Type: "journald"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  otlp:\n", "    traces:\n", "      receivers: [journald]\n      processors: [memory_limiter, zharp_logs, resourcedetection, batch]\n"} {
		if !strings.Contains(res.config, want) {
			t.Errorf("missing %q:\n%s", want, res.config)
		}
	}
	validateMerged(t, res.config)
}

// An install from before v1.6 has no inventory and no inventory_file: the update lists what the
// config already collects and points the exporter at the inventory, so the host page shows it.
func TestMergeBackfillsInventoryForPreV16Install(t *testing.T) {
	fakeHost(t)
	before := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true,
		logSources: []LogSource{{Name: "syslog", Paths: []string{"/var/log/syslog"}}, {Name: "myapp", Paths: []string{"/srv/myapp/*.log"}}}})
	lines := strings.Split(before, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.Contains(l, "inventory_file:") {
			kept = append(kept, l)
		}
	}
	before = strings.Join(kept, "\n")
	before, err := addReceiverToContent(before, "postgresql/orders", strings.Replace(pgBlock, "  postgresql:", "  postgresql/orders:", 1))
	if err != nil {
		t.Fatal(err)
	}

	res, err := mergeSources(before, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.config, "\nexporters:\n  zharp:\n    inventory_file: ") {
		t.Errorf("inventory_file not added under the zharp exporter:\n%s", section(res.config, "exporters", "service"))
	}
	types := map[string]InventoryItem{}
	for _, it := range res.inventory {
		types[it.Type] = it
	}
	for _, want := range []string{"host", "apm", "pg:orders", "syslog", "log:myapp"} {
		if it, ok := types[want]; !ok || it.Status != invCollecting {
			t.Errorf("inventory is missing %q as collecting: %+v", want, res.inventory)
		}
	}
	if got := types["pg:orders"].Label; got != "PostgreSQL (orders)" {
		t.Errorf("label %q", got)
	}
	if len(res.added) != 0 {
		t.Errorf("already-collected sources reported as added: %+v", res.added)
	}
	validateMerged(t, res.config)

	again, err := mergeSources(res.config, res.inventory, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.config != res.config || len(again.inventory) != len(res.inventory) {
		t.Error("a second run changed something")
	}
}

func writeInstall(t *testing.T) updatePaths {
	t.Helper()
	dir := t.TempDir()
	p := updatePaths{config: filepath.Join(dir, "config.yaml"), env: filepath.Join(dir, "env"), inventory: filepath.Join(dir, "inventory.json")}
	if err := os.WriteFile(p.config, []byte(installedConfig(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.env, []byte("ZHARP_API_KEY=zh_live_123\nPG_PASSWORD=s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeInventoryTo(p.inventory, installedInventory); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyUpdateBacksUpAndKeepsSecrets(t *testing.T) {
	fakeHost(t)
	detectFn = func() []ServiceOption { return newerDetections }
	p := writeInstall(t)
	origConfig, _ := os.ReadFile(p.config)
	origEnv, _ := os.ReadFile(p.env)

	now := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	out, err := applyUpdate(p, applyOptions{from: "v1.7.0", appLogs: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !out.configChanged || !out.inventoryChanged {
		t.Fatalf("nothing changed: %+v", out)
	}
	if env, _ := os.ReadFile(p.env); !bytes.Equal(env, origEnv) {
		t.Errorf("secrets changed:\n%s", env)
	}
	if bak, err := os.ReadFile(p.config + ".bak-20261021-093000"); err != nil || !bytes.Equal(bak, origConfig) {
		t.Errorf("config backup: %v", err)
	}
	if bak, err := os.ReadFile(p.env + ".bak-20261021-093000"); err != nil || !bytes.Equal(bak, origEnv) {
		t.Errorf("env backup: %v", err)
	}
	if got, _ := os.ReadFile(p.config); !strings.Contains(string(got), "\n  journald:\n") {
		t.Error("journald not written")
	}

	// Run again: nothing to do, no new backups.
	later := now.Add(time.Hour)
	again, err := applyUpdate(p, applyOptions{appLogs: true}, later)
	if err != nil {
		t.Fatal(err)
	}
	if again.configChanged || again.inventoryChanged || len(again.backups) != 0 {
		t.Errorf("second run: %+v", again)
	}
	if fileExists(p.config + ".bak-20261021-103000") {
		t.Error("second run backed up again")
	}
}

func TestApplyUpdateRestoresWhenTheConfigIsRejected(t *testing.T) {
	fakeHost(t)
	detectFn = func() []ServiceOption { return newerDetections }
	p := writeInstall(t)
	origConfig, _ := os.ReadFile(p.config)
	origInv, _ := os.ReadFile(p.inventory)

	// The merged config is rejected, the one on the host is fine: keep the host as it was.
	validateConfigFn = func(path, _ string) error {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), "journald") {
			return errors.New("bad journald")
		}
		return nil
	}
	out, err := applyUpdate(p, applyOptions{from: "v1.7.0", appLogs: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.warning == "" || out.configChanged || len(out.res.added) != 0 {
		t.Errorf("outcome: %+v", out)
	}
	if got, _ := os.ReadFile(p.config); !bytes.Equal(got, origConfig) {
		t.Error("config not restored")
	}
	if got, _ := os.ReadFile(p.inventory); !bytes.Equal(got, origInv) {
		t.Error("inventory not restored")
	}

	// Even the config as it was doesn't load in the new binary: the binary has to go back.
	validateConfigFn = func(string, string) error { return errors.New("unknown receiver") }
	_, err = applyUpdate(p, applyOptions{from: "v1.7.0", appLogs: true}, time.Now().Add(time.Minute))
	if !errors.Is(err, errConfigRejected) {
		t.Errorf("err = %v, want errConfigRejected", err)
	}
	if got, _ := os.ReadFile(p.config); !bytes.Equal(got, origConfig) {
		t.Error("config not restored")
	}
}

func TestReleaseAsset(t *testing.T) {
	for _, c := range []struct{ goos, goarch, asset, inner string }{
		{"linux", "amd64", "zharp-collector-v1.8.0-linux-amd64.tar.gz", "zharp-collector-linux-amd64"},
		{"linux", "arm64", "zharp-collector-v1.8.0-linux-arm64.tar.gz", "zharp-collector-linux-arm64"},
		{"darwin", "amd64", "zharp-collector-v1.8.0-darwin-amd64.tar.gz", "zharp-collector-darwin-amd64"},
		{"darwin", "arm64", "zharp-collector-v1.8.0-darwin-arm64.tar.gz", "zharp-collector-darwin-arm64"},
		{"windows", "amd64", "zharp-collector-v1.8.0-windows-amd64.exe", ""},
	} {
		asset, inner, err := releaseAsset("v1.8.0", c.goos, c.goarch)
		if err != nil || asset != c.asset || inner != c.inner {
			t.Errorf("%s/%s: %q %q %v", c.goos, c.goarch, asset, inner, err)
		}
	}
	for _, c := range [][2]string{{"windows", "arm64"}, {"linux", "386"}, {"freebsd", "amd64"}} {
		if _, _, err := releaseAsset("v1.8.0", c[0], c[1]); err == nil {
			t.Errorf("%s/%s should have no asset", c[0], c[1])
		}
	}
}

func TestVersionComparison(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.7.0", "v1.8.0", -1, true},
		{"1.8.0", "v1.8.0", 0, true},
		{"v1.10.0", "v1.9.3", 1, true},
		{"v2.0.0", "v1.99.99", 1, true},
		{"v1.8.0-rc1", "v1.8.0", 0, true},
		{"dev", "v1.8.0", 0, false},
	} {
		cmp, ok := compareVersions(c.a, c.b)
		if cmp != c.cmp || ok != c.ok {
			t.Errorf("%s vs %s: %d %v", c.a, c.b, cmp, ok)
		}
	}
	for _, c := range []struct {
		current, release string
		pinned, want     bool
	}{
		{"v1.7.0", "v1.8.0", false, true},
		{"v1.8.0", "v1.8.0", false, false},
		{"v1.9.0", "v1.8.0", false, false}, // newer than the latest release: leave it
		{"v1.9.0", "v1.8.0", true, true},   // pinned: go to it
		{"dev", "v1.8.0", false, true},
	} {
		if got := needsDownload(c.current, c.release, c.pinned); got != c.want {
			t.Errorf("%s → %s pinned=%v: %v", c.current, c.release, c.pinned, got)
		}
	}
}

func TestSwapBinary(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "zharp-collector")
			_ = os.WriteFile(target, []byte("old"), 0o755)
			_ = os.WriteFile(target+".old", []byte("left over"), 0o755)
			newFile := filepath.Join(dir, ".zharp-collector-update-1")
			_ = os.WriteFile(newFile, []byte("new"), 0o755)

			backup, err := swapBinary(target, newFile, goos)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(target); string(got) != "new" {
				t.Errorf("target = %q", got)
			}
			if got, _ := os.ReadFile(backup); string(got) != "old" {
				t.Errorf("backup = %q", got)
			}
			if fileExists(newFile) {
				t.Error("temporary file left")
			}

			if err := rollbackBinary(target, backup, goos); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(target); string(got) != "old" {
				t.Errorf("after rollback target = %q", got)
			}

			_ = os.WriteFile(target+".old", []byte("x"), 0o755)
			_ = os.WriteFile(target+".old-123", []byte("x"), 0o755)
			cleanupOldBinaryAt(target)
			if fileExists(target+".old") || fileExists(target+".old-123") || !fileExists(target) {
				t.Error("cleanup removed the wrong files")
			}
		})
	}
}

func TestDownloadRelease(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "zharp-collector-linux-arm64", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("binary"))
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v1.8.0","name":"v1.8.0"}`))
		case "/dl/v1.8.0/zharp-collector-v1.8.0-linux-arm64.tar.gz":
			_, _ = w.Write(archive.Bytes())
		case "/dl/v1.8.0/zharp-collector-v1.8.0-windows-amd64.exe":
			_, _ = w.Write([]byte("MZexe"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer func(a, d string) { releaseAPIBase, releaseDownloadBase = a, d }(releaseAPIBase, releaseDownloadBase)
	releaseAPIBase, releaseDownloadBase = srv.URL+"/api", srv.URL+"/dl"

	if tag, err := resolveRelease(""); err != nil || tag != "v1.8.0" {
		t.Errorf("latest: %q %v", tag, err)
	}
	if tag, _ := resolveRelease("1.7.0"); tag != "v1.7.0" {
		t.Errorf("pinned: %q", tag)
	}

	dir := t.TempDir()
	f, err := downloadRelease("v1.8.0", "linux", "arm64", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "binary" || filepath.Dir(f) != dir {
		t.Errorf("linux: %s %q", f, got)
	}
	f, err = downloadRelease("v1.8.0", "windows", "amd64", dir)
	if err != nil || !strings.HasSuffix(f, ".exe") {
		t.Fatalf("windows: %s %v", f, err)
	}

	// A release without the asset: an error, and nothing left behind.
	before, _ := os.ReadDir(dir)
	if _, err := downloadRelease("v9.9.9", "linux", "amd64", dir); err == nil {
		t.Error("missing asset downloaded")
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Error("failed download left a file")
	}

	// GitHub down.
	releaseAPIBase = "http://127.0.0.1:1"
	if _, err := resolveRelease(""); err == nil {
		t.Error("unreachable API should fail")
	}
}

func TestReceiverLabels(t *testing.T) {
	rs, err := configReceivers(installedConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(receiverLabels(rs), ", "); got != "Host metrics, myapp logs, Application traces, PostgreSQL" {
		t.Errorf("labels: %s", got)
	}
}
