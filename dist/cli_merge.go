package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The merge step of `zharp-collector update`, always run by the newest binary so its detection
// is the one used. What this version finds on the host is compared with what the config already
// collects, and only what is missing is added — through the helpers `add` uses. Nothing that is
// configured is removed or rewritten: receivers, processors, exporters, rules and secrets stay
// byte for byte, and the pipelines only gain receivers.

// configReceiver is one receiver of a config on disk and, for a file tail, what it reads.
type configReceiver struct {
	ID      string
	Include []string
}

// configReceivers lists a config's receivers in the order they appear.
func configReceivers(content string) ([]configReceiver, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a collector config")
	}
	root := doc.Content[0]
	var out []configReceiver
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "receivers" {
			continue
		}
		recv := root.Content[i+1]
		for j := 0; j+1 < len(recv.Content); j += 2 {
			r := configReceiver{ID: recv.Content[j].Value}
			body := recv.Content[j+1]
			for k := 0; body.Kind == yaml.MappingNode && k+1 < len(body.Content); k += 2 {
				if body.Content[k].Value == "include" {
					for _, n := range body.Content[k+1].Content {
						r.Include = append(r.Include, n.Value)
					}
				}
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// existingSources answers "is this already collected?" for a config.
type existingSources []configReceiver

// has reports a receiver id, or a named one of its kind ("postgresql" matches "postgresql/orders").
func (e existingSources) has(id string) bool {
	for _, r := range e {
		if r.ID == id || strings.HasPrefix(r.ID, id+"/") {
			return true
		}
	}
	return false
}

// tails reports whether any file receiver already reads file, by its path or a glob.
func (e existingSources) tails(file string) bool {
	file = filepath.ToSlash(file)
	for _, r := range e {
		for _, inc := range r.Include {
			inc = filepath.ToSlash(inc)
			if ok, _ := path.Match(inc, file); ok || inc == file {
				return true
			}
		}
	}
	return false
}

// updateTypeReceivers is the receiver that shows a detected type is configured.
var updateTypeReceivers = map[string]string{
	"docker":      "docker_stats",
	"docker_logs": "docker_logs",
	"journald":    "journald",
	"pm2":         "filelog/pm2",
	"redis":       "redis",
	"pg":          "postgresql",
	"mysql":       "mysql",
	"mongo":       "mongodb",
}

// logSourceTypes maps the file sources planAutomatic tails to their inventory types.
var logSourceTypes = map[string]string{"nginx": "nginx_log", "apache": "apache_log", "syslog": "syslog", "iis": "iis_log"}

// mergeResult is what the merge would make of a config and inventory.
type mergeResult struct {
	config    string
	inventory []InventoryItem
	added     []InventoryItem // newly collecting
	unchanged []string        // what the config already collected
	notAdded  []string        // found, but left as it was on purpose: why, and how to add it
	docker    bool            // the collector's user needs the Docker socket
	stateDir  bool            // a new receiver keeps positions in the state directory
}

// mergeSources adds to content what planAutomatic would set up for detected and isn't there yet.
func mergeSources(content string, inventory []InventoryItem, detected []ServiceOption, appLogs bool) (mergeResult, error) {
	res := mergeResult{config: content, inventory: append([]InventoryItem(nil), inventory...)}
	have, err := configReceivers(content)
	if err != nil {
		return res, fmt.Errorf("could not read the config: %w", err)
	}
	ex := existingSources(have)
	res.unchanged = receiverLabels(have)
	// An install from before v1.6 has no inventory: list what its config already collects, so
	// the host page shows those sources too, not only what this update adds.
	res.inventory = backfillInventory(res.inventory, have)

	// A source the person skipped in the guided install stays skipped.
	optedOut := map[string]bool{}
	for _, it := range inventory {
		if it.Status == invNeedsSetup && it.Detail == skippedDuringInstall {
			optedOut[it.Type] = true
		}
	}

	var pending []ServiceOption
	syslog := false
	for _, d := range detected {
		if isAppLogType(d.Type) && !appLogs {
			continue
		}
		if id, ok := updateTypeReceivers[d.Type]; ok && ex.has(id) {
			continue
		}
		if optedOut[d.Type] {
			res.notAdded = append(res.notAdded, fmt.Sprintf("%s: skipped during install. To add it: %s", inventoryFor(d.Type).Label, addCommand(d.Type)))
			continue
		}
		if d.Type == "journald" && (ex.tails("/var/log/syslog") || ex.tails("/var/log/messages")) {
			// Adding the journal would mean dropping the syslog files `add journal` replaces, and
			// update doesn't take away what is collected.
			res.notAdded = append(res.notAdded, "Service logs (journal): /var/log/syslog is already collected and carries the same lines. To switch to the journal: "+addCommand("journald"))
			continue
		}
		if d.Type == "syslog" {
			syslog = true // which files depends on the journal, below
			continue
		}
		pending = append(pending, d)
	}

	plan := planAutomaticWith(pending, appLogs)
	addedTypes := map[string]bool{}
	journal := ex.has("journald")
	for _, r := range plan.logReceivers {
		journal = journal || r.ID == "journald"
	}
	if syslog {
		plan.addSystemLogs(journal)
	}
	skip := map[string]bool{} // inventory types to leave as they are
	if !ex.has("hostmetrics") {
		skip["host"] = true
	}

	// Metric receivers. The first is hostmetrics, always there; the rest pair with their blocks.
	for i, id := range plan.metricsReceivers[1:] {
		next, err := addReceiverToContent(res.config, id, plan.receiverBlocks[i])
		if err != nil {
			return res, err
		}
		res.config = next
		switch id {
		case "docker_stats":
			addedTypes["docker"] = true
			res.docker = true
		case "redis":
			addedTypes["redis"] = true
		}
	}

	// Log files. One already read under another name (a custom source, a glob) is left alone.
	hadLogsPipeline := logsPipelineRe.MatchString(content)
	logs := false
	for _, src := range plan.logSources {
		typ := logSourceTypes[src.Name]
		covered := ex.has("filelog/" + src.Name)
		for _, p := range src.Paths {
			covered = covered || ex.tails(p)
		}
		if covered {
			skip[typ] = true
			continue
		}
		res.config = addLogSourcesToContent(res.config, []LogSource{src})
		addedTypes[typ] = true
		logs = true
	}

	// Application logs: containers, the journal, PM2.
	for _, r := range plan.logReceivers {
		res.config = addLogReceiverContent(res.config, r)
		res.stateDir = res.stateDir || r.Storage || r.ID == "docker_logs"
		res.docker = res.docker || r.ID == "docker_logs"
		switch r.ID {
		case "docker_logs":
			addedTypes["docker_logs"] = true
		case "journald":
			addedTypes["journald"] = true
		case "filelog/pm2":
			addedTypes["pm2"] = true
		}
		logs = true
	}
	// A logs pipeline update creates gets the rules processor, as install's does. One that was
	// there keeps its processors as they are.
	if logs && !hadLogsPipeline {
		res.config = ensureLogRules(res.config)
	}

	// Application traces on localhost, as every install since traces has had.
	if plan.tracing && !ex.has("otlp") {
		res.config = addTracing(res.config)
		addedTypes["apm"] = true
	} else {
		skip["apm"] = true
	}

	pm2Setup := false
	for _, it := range plan.inventory {
		pm2Setup = pm2Setup || it.Type == "pm2:setup"
		if skip[it.Type] {
			continue
		}
		res.inventory = mergeInventoryItem(res.inventory, it)
		if it.Status == invCollecting && addedTypes[it.Type] {
			res.added = append(res.added, it)
		}
	}
	if addedTypes["pm2"] && !pm2Setup {
		res.inventory = withoutInventoryType(res.inventory, "pm2:setup") // everything is readable now
	}
	res.config = withInventoryFile(res.config)
	return res, nil
}

// backfillInventory adds a "collecting" entry for every source the config collects that the
// inventory doesn't list yet. Entries already there are kept as they are.
func backfillInventory(items []InventoryItem, have []configReceiver) []InventoryItem {
	listed := map[string]bool{}
	for _, it := range items {
		listed[it.Type] = true
	}
	add := func(it InventoryItem) {
		if !listed[it.Type] {
			listed[it.Type] = true
			items = append(items, it)
		}
	}
	dbTypes := map[string]string{"postgresql": "pg", "mysql": "mysql", "redis": "redis", "mongodb": "mongo"}
	for _, r := range have {
		base, name, _ := strings.Cut(r.ID, "/")
		switch {
		case base == "hostmetrics":
			add(InventoryItem{Type: "host", Label: "Host metrics", Status: invCollecting, Detail: "CPU, memory, disk, network and load"})
		case base == "otlp":
			add(InventoryItem{Type: "apm", Label: "Application traces", Status: invCollecting, Detail: "OpenTelemetry on 127.0.0.1:4317 (gRPC) and :4318 (HTTP)"})
		case dbTypes[base] != "":
			it := inventoryFor(dbTypes[base])
			if name != "" {
				it.Type = it.Type + ":" + name
				it.Label = fmt.Sprintf("%s (%s)", it.Label, name)
			}
			add(it)
		case base == "docker_stats":
			add(inventoryFor("docker"))
		case base == "docker_logs" || base == "journald":
			add(inventoryFor(base))
		case base == "filelog" && name == "pm2":
			add(inventoryFor("pm2"))
		case base == "filelog" && logSourceTypes[name] != "":
			add(inventoryFor(logSourceTypes[name]))
		case base == "filelog" && name != "":
			add(InventoryItem{Type: "log:" + name, Label: name + " logs", Status: invCollecting, Detail: strings.Join(r.Include, ", ")})
		}
	}
	return items
}

// withInventoryFile points the Zharp exporter at the inventory when the config doesn't — an
// install from before v1.6 — so the heartbeat sends it and the host page can show it. It is the
// one line the update adds to an existing exporter.
func withInventoryFile(content string) string {
	if strings.Contains(content, "inventory_file:") {
		return content
	}
	const exporter = "\nexporters:\n  zharp:\n"
	i := strings.Index(content, exporter)
	if i < 0 {
		return content
	}
	at := i + len(exporter)
	return content[:at] + fmt.Sprintf("    inventory_file: %q\n", installInventoryFile()) + content[at:]
}

// mergeInventoryItem records it, unless the same source is already collecting: a "collecting"
// entry is never replaced, a "needs setup" one is (with what was found now).
func mergeInventoryItem(items []InventoryItem, it InventoryItem) []InventoryItem {
	for i := range items {
		if items[i].Type != it.Type {
			continue
		}
		if items[i].Status != invCollecting {
			items[i] = it
		}
		return items
	}
	return append(items, it)
}

func withoutInventoryType(items []InventoryItem, typ string) []InventoryItem {
	var keep []InventoryItem
	for _, it := range items {
		if it.Type != typ {
			keep = append(keep, it)
		}
	}
	return keep
}

// addTracing adds the OTLP receiver and the traces pipeline.
func addTracing(content string) string {
	content = strings.Replace(content, "\nprocessors:", "\n"+strings.TrimRight(otlpReceiverBlock, "\n")+"\n\nprocessors:", 1)
	if i := strings.Index(content, "\n    metrics:\n"); i >= 0 {
		return content[:i+1] + tracesPipelineBlock + content[i+1:]
	}
	return strings.Replace(content, "\n  pipelines:\n", "\n  pipelines:\n"+tracesPipelineBlock, 1)
}

// receiverLabels names what a config collects, for the summary.
func receiverLabels(rs []configReceiver) []string {
	names := map[string]string{
		"hostmetrics": "Host metrics", "postgresql": "PostgreSQL", "mysql": "MySQL / MariaDB",
		"redis": "Redis", "mongodb": "MongoDB", "docker_stats": "Docker", "docker_logs": "Docker container logs",
		"journald": "Service logs (journal)", "otlp": "Application traces",
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		base, name, _ := strings.Cut(r.ID, "/")
		label := names[base]
		switch {
		case base == "sqlquery":
			continue // MySQL's connection limit, part of MySQL
		case base == "filelog" && name == "pm2":
			label = "PM2 app logs"
		case base == "filelog" && name != "":
			label = name + " logs"
		case label == "":
			label = r.ID
		case name != "":
			label += " (" + name + ")"
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

// addCommand is the command that sets a source up by hand ("" when there is none).
func addCommand(typ string) string {
	base, name, _ := strings.Cut(typ, ":")
	cmd := map[string]string{
		"pg":          "add postgres --user zharp_monitor --password PASSWORD --db DATABASE",
		"mysql":       "add mysql --user zharp_monitor --password PASSWORD",
		"mongo":       "add mongo --user zharp_monitor --password PASSWORD",
		"redis":       "add redis --password PASSWORD",
		"nginx_log":   "add nginx --access-log PATH",
		"apache_log":  "add apache --access-log PATH",
		"syslog":      "add syslog",
		"iis_log":     "add iis --log-folder PATH",
		"docker":      "add docker",
		"docker_logs": "add docker-logs",
		"journald":    "add journal",
		"pm2":         "add pm2",
	}[base]
	if cmd == "" {
		return ""
	}
	switch base {
	case "pg", "mysql", "mongo", "redis":
		if name != "" {
			cmd += " --name " + name
		}
	}
	return cliPrefix() + "zharp-collector " + cmd
}

// setupHint is how to finish a "needs setup" item: the commands in its detail (PM2), or `add`.
func setupHint(it InventoryItem) (why, how string) {
	if i := strings.Index(it.Detail, ". Run: "); i >= 0 {
		return it.Detail[:i], it.Detail[i+len(". Run: "):]
	}
	return it.Detail, addCommand(it.Type)
}

func cliPrefix() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "sudo "
}

// ── applying it ─────────────────────────────────────────────────────────────

// updatePaths are the files the merge reads and writes.
type updatePaths struct{ config, env, inventory string }

func installedPaths() updatePaths {
	return updatePaths{config: installConfigFile(), env: installEnvFile(), inventory: installInventoryFile()}
}

// What applyUpdate does to the host beyond its files; tests replace these.
var (
	detectFn         = detectServices
	validateConfigFn = validateWithCollector
	restartFn        = svcRestart
	grantDockerFn    = grantDockerAccess
	prepareStateFn   = prepareStateDir
)

// applyOptions are the merge step's flags.
type applyOptions struct {
	from    string // version the binary was updated from; "" when it wasn't replaced
	appLogs bool   // false with --no-app-logs / ZHARP_NO_APP_LOGS
	dryRun  bool   // `update --check`: report, change nothing
}

// binaryChanged is true when this run follows a new binary (the old config must load in it).
func (o applyOptions) binaryChanged() bool {
	return o.from != "" && strings.TrimPrefix(o.from, "v") != strings.TrimPrefix(version, "v")
}

// applyOutcome is what applyUpdate did.
type applyOutcome struct {
	res              mergeResult
	configChanged    bool
	inventoryChanged bool
	backups          []string
	warning          string // the new sources were left out, and why
}

// errConfigRejected means this binary can't load the config already on the host, so the
// previous binary has to stay (update puts it back).
var errConfigRejected = errors.New("the installed config does not load in this version")

// applyUpdate merges what this host runs into its config and inventory. The files are backed
// up first (<file>.bak-<time>), and the new config must pass the collector's own validation —
// otherwise everything is put back as it was.
func applyUpdate(p updatePaths, opts applyOptions, now time.Time) (applyOutcome, error) {
	var out applyOutcome
	orig, err := os.ReadFile(p.config)
	if err != nil {
		return out, fmt.Errorf("no config at %s; run 'zharp-collector install' first", p.config)
	}
	inventory := readInventoryFrom(p.inventory)
	content := string(orig)

	// --no-app-logs is remembered in the config, so later updates keep app logs out too.
	appLogs := opts.appLogs && !strings.Contains(content, noAppLogsMarker)
	if !opts.appLogs && !strings.Contains(content, noAppLogsMarker) {
		content = noAppLogsMarker + content
	}

	if opts.dryRun {
		// Planning PM2 hands out read access; a check must not.
		defer func(f func(string) error) { grantPM2Access = f }(grantPM2Access)
		grantPM2Access = func(string) error { return nil }
	}
	res, err := mergeSources(content, inventory, detectFn(), appLogs)
	if err != nil {
		return out, err
	}
	out.res = res
	out.configChanged = res.config != string(orig)
	out.inventoryChanged = !reflect.DeepEqual(inventory, res.inventory)
	if opts.dryRun {
		return out, nil
	}

	if !out.configChanged && !out.inventoryChanged {
		if opts.binaryChanged() {
			if err := validateConfigFn(p.config, p.env); err != nil {
				return out, fmt.Errorf("%w: %v", errConfigRejected, err)
			}
		}
		return out, nil
	}

	// Back up everything this writes, and the secrets beside it.
	stamp := now.Format("20060102-150405")
	type saved struct{ file, backup string }
	var saves []saved
	for _, f := range []string{p.config, p.env, p.inventory} {
		if !fileExists(f) {
			continue
		}
		b := f + ".bak-" + stamp
		if err := copyFile(f, b); err != nil {
			return out, fmt.Errorf("could not back up %s: %w", f, err)
		}
		saves = append(saves, saved{f, b})
		out.backups = append(out.backups, b)
	}
	restore := func() {
		for _, s := range saves {
			_ = copyFile(s.backup, s.file)
		}
		if !fileExists(p.inventory + ".bak-" + stamp) {
			_ = os.Remove(p.inventory) // there was none
		}
	}

	if out.configChanged {
		if res.docker {
			grantDockerFn()
		}
		if res.stateDir {
			if err := prepareStateFn(); err != nil {
				uiWarn("Could not create " + installStateDir() + ": " + err.Error())
			}
		}
		if err := os.WriteFile(p.config, []byte(res.config), 0o644); err != nil {
			restore()
			return out, fmt.Errorf("could not write %s: %w", p.config, err)
		}
		if err := validateConfigFn(p.config, p.env); err != nil {
			restore()
			// The new sources are the problem if the config as it was still loads.
			if opts.binaryChanged() {
				if err2 := validateConfigFn(p.config, p.env); err2 != nil {
					return out, fmt.Errorf("%w: %v", errConfigRejected, err2)
				}
			}
			out.warning = "The new sources were not added — the collector rejected the merged config: " + err.Error()
			out.configChanged, out.inventoryChanged = false, false
			out.res.added, out.res.inventory = nil, inventory
			return out, nil
		}
	}
	if out.inventoryChanged {
		if err := writeInventoryTo(p.inventory, res.inventory); err != nil {
			uiWarn("Could not write the inventory: " + err.Error())
		}
	}
	return out, nil
}

// validateWithCollector runs this binary's `validate` on the config, with the secrets it refers to.
func validateWithCollector(configFile, envFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "validate", "--config", "file:"+configFile)
	cmd.Env = append(os.Environ(), readEnvFile(envFile)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readEnvFile returns the KEY=VALUE lines of the secrets file.
func readEnvFile(envFile string) []string {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return nil
	}
	var vars []string
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") && strings.Contains(l, "=") {
			vars = append(vars, l)
		}
	}
	return vars
}

func copyFile(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, fi.Mode().Perm())
}

// runApply is `update --apply` (and the in-place half of `update` and `update --check`): merge,
// restart when anything changed, and say what happened.
func runApply(opts applyOptions) {
	fmt.Println()
	uiInfo("Scanning this server...")
	out, err := applyUpdate(installedPaths(), opts, time.Now())
	if errors.Is(err, errConfigRejected) {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		fmt.Fprintln(os.Stderr, "      Nothing was changed.")
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		fmt.Fprintln(os.Stderr, "      Nothing was changed.")
		os.Exit(1)
	}

	if !opts.dryRun && (out.configChanged || opts.binaryChanged()) {
		uiInfo("Restarting service to apply changes...")
		if err := restartFn(); err != nil {
			uiWarn("Restart failed: " + err.Error())
			uiDimMsg("Restart manually: " + cliPrefix() + "zharp-collector restart")
		}
	}
	printUpdateSummary(out, opts)
}

// printUpdateSummary prints what changed, what was already there and what is left to do.
func printUpdateSummary(out applyOutcome, opts applyOptions) {
	uiSection("Summary")
	switch {
	case opts.dryRun:
	case opts.binaryChanged() && opts.from == "unknown":
		uiOK("Updated to " + strings.TrimPrefix(version, "v"))
	case opts.binaryChanged():
		uiOK(fmt.Sprintf("Updated %s → %s", strings.TrimPrefix(opts.from, "v"), strings.TrimPrefix(version, "v")))
	default:
		uiOK("Version " + strings.TrimPrefix(version, "v") + " (no new binary)")
	}

	verb := "Added"
	if opts.dryRun {
		verb = "Would add"
	}
	if len(out.res.added) == 0 {
		uiDimMsg(verb + ": nothing new on this host.")
	} else {
		fmt.Printf("  %s%s:%s\n", colBold, verb, colReset)
		for _, it := range out.res.added {
			uiOK(fmt.Sprintf("%-24s %s", it.Label, addedDetail(it)))
		}
	}
	if len(out.res.unchanged) > 0 {
		fmt.Printf("  %sUnchanged:%s %s\n", colBold, colReset, strings.Join(out.res.unchanged, ", "))
	}

	var waiting []InventoryItem
	for _, it := range out.res.inventory {
		if it.Status == invNeedsSetup {
			waiting = append(waiting, it)
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].Label < waiting[j].Label })
	if len(waiting) > 0 {
		fmt.Printf("  %sNeeds setup:%s\n", colBold, colReset)
		for _, it := range waiting {
			why, how := setupHint(it)
			uiWarn(fmt.Sprintf("%-24s %s", it.Label, why))
			if how != "" {
				uiDimMsg(how)
			}
		}
	}
	for _, n := range out.res.notAdded {
		uiWarn("Not added — " + n)
	}
	if out.warning != "" {
		uiWarn(out.warning)
	}
	if len(out.backups) > 0 {
		uiDimMsg("Backups: " + strings.Join(out.backups, ", "))
	}
	fmt.Println()
}

// addedDetail shortens an inventory detail for the summary ("2 apps: api, worker" for PM2).
func addedDetail(it InventoryItem) string {
	if it.Type == "pm2" {
		apps := strings.Split(it.Detail, ", ")
		if len(apps) == 1 {
			return "1 app: " + it.Detail
		}
		return fmt.Sprintf("%d apps: %s", len(apps), it.Detail)
	}
	return it.Detail
}
