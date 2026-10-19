package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// cliAdd handles `zharp-collector add <type> [flags]`
//
// Examples:
//
//	zharp-collector add postgres --host localhost --port 5432 --user zharp_monitor --password secret --db myapp
//	zharp-collector add postgres --name orders --host 10.0.0.12 --user zharp_monitor --password secret --db orders
//	zharp-collector add docker
//	zharp-collector add logs --path /var/log/myapp/app.log --service myapp
func cliAdd() {
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "  [!] This command must be run as root. Try: sudo zharp-collector add")
		os.Exit(1)
	}

	if len(os.Args) < 3 {
		printAddUsage()
		os.Exit(1)
	}

	svcType := normalizeAddArg(os.Args[2])
	if svcType == "" {
		fmt.Fprintf(os.Stderr, "  [!] Unknown type %q. Run 'zharp-collector help' to see valid types.\n", os.Args[2])
		os.Exit(1)
	}

	configFile := installConfigFile()
	envFile := installEnvFile()
	if _, err := os.Stat(configFile); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] No config found at %s. Run 'zharp-collector install' first.\n", configFile)
		os.Exit(1)
	}

	args := os.Args[3:]
	receiver, block, logSources, envVars, err := parseAddArgs(svcType, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		fmt.Fprintln(os.Stderr)
		printAddTypeUsage(svcType)
		os.Exit(1)
	}
	name := databaseNameFlag(svcType, args)

	if name != "" {
		applyNamedDatabase(configFile, envFile, receiver, block, envVars, name)
	} else {
		if block != "" && receiverConfigured(configFile, receiver) {
			// The engine's default receiver is already there: its settings stay as they were
			// (only the password in the env file is updated). A second database of this engine
			// needs a name of its own.
			uiWarn(fmt.Sprintf("%s is already configured on this host; its settings were kept.", inventoryFor(svcType).Label))
			uiDimMsg(fmt.Sprintf("To monitor another database of this engine, add it with a name: zharp-collector add %s --name NAME ...", os.Args[2]))
		}
		applyAddition(configFile, envFile, receiver, block, logSources, envVars)
	}

	// Tell the dashboard on the next heartbeat. Custom log sources are kept apart by name, and so
	// are named databases.
	item := inventoryFor(svcType)
	if svcType == "custom_log" && len(logSources) > 0 {
		item.Type = "log:" + logSources[0].Name
		item.Label = logSources[0].Name + " logs"
		item.Detail = strings.Join(logSources[0].Paths, ", ")
	}
	if name != "" {
		item.Type = svcType + ":" + name
		item.Label = fmt.Sprintf("%s (%s)", item.Label, name)
	}
	markInventory(item)

	uiInfo("Restarting service to apply changes...")
	if err := svcRestart(); err != nil {
		uiWarn("Restart failed: " + err.Error())
		uiDimMsg("Restart manually: zharp-collector restart")
	} else {
		uiOK("Done. Data will appear in your Zharp dashboard within a minute.")
	}
	fmt.Println()
}

// parseAddArgs parses flags for the given type and returns the receiver config.
func parseAddArgs(svcType string, args []string) (receiver, block string, logSources []LogSource, envVars map[string]string, err error) {
	envVars = map[string]string{}
	fs := flag.NewFlagSet("add "+svcType, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	switch svcType {
	case "pg":
		host := fs.String("host", "localhost", "")
		port := fs.String("port", "5432", "")
		user := fs.String("user", "zharp_monitor", "")
		pass := fs.String("password", "", "")
		db := fs.String("db", "postgres", "")
		name := fs.String("name", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if *pass == "" {
			err = fmt.Errorf("--password is required for postgres")
			return
		}
		if err = checkDatabaseName(*name); err != nil {
			return
		}
		id, env := namedIDs("postgresql", "PG_PASSWORD", *name)
		receiver = id
		block = fmt.Sprintf(`
  %s:
    endpoint: %s:%s
    username: %s
    password: "${env:%s}"
    databases:
      - %s
    collection_interval: 30s
    tls:
      insecure: true
`, id, *host, *port, *user, env, *db)
		envVars[env] = *pass

	case "mysql":
		host := fs.String("host", "localhost", "")
		port := fs.String("port", "3306", "")
		user := fs.String("user", "zharp_monitor", "")
		pass := fs.String("password", "", "")
		name := fs.String("name", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if *pass == "" {
			err = fmt.Errorf("--password is required for mysql")
			return
		}
		if err = checkDatabaseName(*name); err != nil {
			return
		}
		_, env := namedIDs("mysql", "MYSQL_PASSWORD", *name)
		receiver, block = mysqlReceiversNamed(*host, *port, *user, *name)
		envVars[env] = *pass

	case "redis":
		endpoint := fs.String("endpoint", "localhost:6379", "")
		pass := fs.String("password", "", "")
		name := fs.String("name", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if err = checkDatabaseName(*name); err != nil {
			return
		}
		id, env := namedIDs("redis", "REDIS_PASSWORD", *name)
		receiver = id
		if *pass != "" {
			block = fmt.Sprintf(`
  %s:
    endpoint: %s
    password: "${env:%s}"
    collection_interval: 30s
`, id, *endpoint, env)
			envVars[env] = *pass
		} else {
			block = fmt.Sprintf(`
  %s:
    endpoint: %s
    collection_interval: 30s
`, id, *endpoint)
		}

	case "mongo":
		endpoint := fs.String("endpoint", "localhost:27017", "")
		user := fs.String("user", "zharp_monitor", "")
		pass := fs.String("password", "", "")
		name := fs.String("name", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if *pass == "" {
			err = fmt.Errorf("--password is required for mongo")
			return
		}
		if err = checkDatabaseName(*name); err != nil {
			return
		}
		id, env := namedIDs("mongodb", "MONGO_PASSWORD", *name)
		receiver = id
		block = fmt.Sprintf(`
  %s:
    hosts:
      - endpoint: %s
    username: %s
    password: "${env:%s}"
    collection_interval: 30s
    tls:
      insecure: true
`, id, *endpoint, *user, env)
		envVars[env] = *pass

	case "docker":
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		endpoint := "unix:///var/run/docker.sock"
		if runtime.GOOS == "windows" {
			endpoint = "npipe:////./pipe/docker_engine"
		}
		receiver = "docker_stats"
		block = fmt.Sprintf(`
  docker_stats:
    endpoint: %s
    collection_interval: 30s
    timeout: 20s
`, endpoint)

	case "nginx_log":
		svcName := fs.String("service", "nginx", "")
		accessLog := fs.String("access-log", "", "")
		errorLog := fs.String("error-log", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if *accessLog == "" {
			switch runtime.GOOS {
			case "windows":
				*accessLog = `C:\nginx\logs\access.log`
			case "darwin":
				*accessLog = "/usr/local/var/log/nginx/access.log"
			default:
				*accessLog = "/var/log/nginx/access.log"
			}
		}
		paths := []string{*accessLog}
		if *errorLog != "" {
			paths = append(paths, *errorLog)
		}
		logSources = []LogSource{{Name: sanitizeServiceName(*svcName), Paths: paths}}

	case "apache_log":
		svcName := fs.String("service", "apache", "")
		accessLog := fs.String("access-log", "", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if *accessLog == "" {
			if fileExists("/var/log/httpd/access_log") {
				*accessLog = "/var/log/httpd/access_log"
			} else {
				*accessLog = "/var/log/apache2/access.log"
			}
		}
		logSources = []LogSource{{Name: sanitizeServiceName(*svcName), Paths: []string{*accessLog}}}

	case "syslog":
		svcName := fs.String("service", "syslog", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		var paths []string
		for _, p := range []string{"/var/log/syslog", "/var/log/messages", "/var/log/auth.log"} {
			if fileExists(p) {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			err = fmt.Errorf("no syslog files found (/var/log/syslog, /var/log/messages)")
			return
		}
		logSources = []LogSource{{Name: sanitizeServiceName(*svcName), Paths: paths}}

	case "iis_log":
		svcName := fs.String("service", "iis", "")
		folder := fs.String("log-folder", `C:\inetpub\logs\LogFiles\`, "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		f := *folder
		if !strings.HasSuffix(f, `\`) {
			f += `\`
		}
		logSources = []LogSource{{Name: sanitizeServiceName(*svcName), Paths: []string{f + `**\*.log`}}}

	case "custom_log":
		svcName := fs.String("service", "", "")
		var paths multiFlag
		fs.Var(&paths, "path", "")
		if e := fs.Parse(args); e != nil {
			err = e
			return
		}
		if len(paths) == 0 {
			err = fmt.Errorf("--path is required (repeat for multiple files)")
			return
		}
		name := *svcName
		if name == "" {
			// Try to detect from first file's content, fall back to filename.
			name = detectServiceFromFile(paths[0])
		}
		logSources = []LogSource{{Name: sanitizeServiceName(name), Paths: []string(paths)}}
	}

	return receiver, block, logSources, envVars, nil
}

// knownLogLevels is the set of recognised level tokens (lowercase, no decorators).
var knownLogLevels = map[string]bool{
	"trace": true, "debug": true, "info": true, "notice": true,
	"warn": true, "warning": true, "error": true, "err": true,
	"fatal": true, "crit": true, "critical": true, "panic": true,
}

// stripDecor removes surrounding [], (), whitespace and a trailing colon.
func stripDecor(s string) string {
	s = strings.Trim(s, "[](): \t")
	return strings.TrimSuffix(s, ":")
}

// isDatetimeToken returns true if s starts with an ISO date (YYYY-MM-DD...).
func isDatetimeToken(s string) bool {
	if len(s) < 10 {
		return false
	}
	// Quick digit check before compiling regex.
	matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}`, s)
	return matched
}

// isTimeToken returns true if s starts with HH:MM:SS (space-separated time field).
func isTimeToken(s string) bool {
	matched, _ := regexp.MatchString(`^\d{2}:\d{2}:\d{2}`, s)
	return matched
}

// isLevelToken returns true if s (after stripping decorators) is a known log level.
func isLevelToken(s string) bool {
	return knownLogLevels[strings.ToLower(stripDecor(s))]
}

// cleanServiceToken strips process-ID suffixes ("nginx[1234]:") and decorators.
func cleanServiceToken(s string) string {
	s = regexp.MustCompile(`\[\d+\]:?$`).ReplaceAllString(s, "")
	return stripDecor(s)
}

// detectServiceFromLine parses a single log line looking for the pattern:
//
//	<datetime> <level> <service> ...         (single-token datetime)
//	<date> <time> <level> <service> ...      (space-separated date+time)
//
// Returns the service name, or "" if the line doesn't match either pattern.
func detectServiceFromLine(line string) string {
	fields := strings.Fields(line)

	// Pattern 1: single-token ISO datetime
	// e.g.  2026-06-13T19:05:03Z  INFO  myapp  Starting server
	//        [2026-06-13T19:05:03] [WARN] [api] upstream timeout
	if len(fields) >= 3 && isDatetimeToken(stripDecor(fields[0])) && isLevelToken(fields[1]) {
		name := cleanServiceToken(fields[2])
		if name != "" {
			return name
		}
	}

	// Pattern 2: space-separated date + time
	// e.g.  2026-06-13 19:05:03.456  ERROR  payment-svc  dial tcp refused
	if len(fields) >= 4 && isDatetimeToken(stripDecor(fields[0])) && isTimeToken(fields[1]) && isLevelToken(fields[2]) {
		name := cleanServiceToken(fields[3])
		if name != "" {
			return name
		}
	}

	return ""
}

// detectServiceFromFile reads up to 10 non-empty lines and returns the service
// name from the first line that matches the structured log pattern.
// Falls back to the file's basename without extension.
func detectServiceFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return fileBasename(path)
	}
	defer f.Close()

	checked := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && checked < 10 {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		checked++
		if name := detectServiceFromLine(line); name != "" {
			return sanitizeServiceName(name)
		}
	}
	return fileBasename(path)
}

// fileBasename returns the filename without directory or extension, sanitized.
func fileBasename(path string) string {
	base := filepath.Base(path)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}

// sanitizeServiceName lowercases and replaces non-alphanumeric chars with underscores.
func sanitizeServiceName(name string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	s := re.ReplaceAllString(strings.TrimSpace(name), "_")
	return strings.ToLower(strings.Trim(s, "_"))
}

// multiFlag is a flag.Value that accumulates repeated --flag values.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// applyAddition writes the new receiver/logs to the config and env files.
func applyAddition(configFile, envFile, receiver, block string, logSources []LogSource, envVars map[string]string) {
	if block != "" {
		if err := addReceiverToConfig(configFile, receiver, block); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] Could not update config: %v\n", err)
			os.Exit(1)
		}
	}
	if len(logSources) > 0 {
		if err := addLogSourcesToConfig(configFile, logSources); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] Could not update log sources: %v\n", err)
			os.Exit(1)
		}
	}
	if len(envVars) > 0 {
		if err := appendEnvVars(envFile, envVars); err != nil {
			uiWarn("Could not update env file: " + err.Error())
		}
	}
	uiOK("Config updated → " + configFile)
}

// addReceiverToConfig inserts a receiver YAML block and appends the receiver
// name to the metrics pipeline receivers list.
func addReceiverToConfig(configFile, receiver, block string) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	content := string(data)

	block = strings.TrimRight(block, "\n") + "\n"

	marker := "\nprocessors:"
	if !strings.Contains(content, marker) {
		return fmt.Errorf("could not find processors section in config")
	}
	// A source can need more than one receiver (MySQL adds a query for its connection limit);
	// they come comma-separated and share one block, so the first name decides if it's there.
	var names []string
	for _, r := range strings.Split(receiver, ",") {
		if r = strings.TrimSpace(r); r != "" {
			names = append(names, r)
		}
	}
	if len(names) > 0 && !strings.Contains(content, "\n  "+names[0]+":") {
		content = strings.Replace(content, marker, "\n"+block+marker, 1)
	}

	// Append receiver name to the metrics pipeline receivers list.
	re := regexp.MustCompile(`([ \t]+metrics:[ \t]*\r?\n[ \t]+receivers:[ \t]*\[)([^\]]*)\]`)
	content = re.ReplaceAllStringFunc(content, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		existing := strings.TrimSpace(sub[2])
		present := map[string]bool{}
		for _, r := range strings.Split(existing, ",") {
			present[strings.TrimSpace(r)] = true
		}
		for _, name := range names {
			if present[name] {
				continue
			}
			if existing == "" {
				existing = name
			} else {
				existing += ", " + name
			}
		}
		return sub[1] + existing + "]"
	})

	return os.WriteFile(configFile, []byte(content), 0o644)
}

// addLogSourcesToConfig adds named filelog/<name> receivers for each LogSource,
// creating the logs pipeline if needed.
func addLogSourcesToConfig(configFile string, sources []LogSource) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	content := string(data)

	for _, src := range sources {
		receiverKey := "filelog/" + src.Name
		receiverHeader := "  " + receiverKey + ":"

		if strings.Contains(content, receiverHeader) {
			// Receiver already exists — append new paths before include_file_path.
			idx := strings.Index(content, receiverHeader)
			rest := content[idx:]
			ipIdx := strings.Index(rest, "    include_file_path:")
			if ipIdx >= 0 {
				newLines := ""
				for _, p := range src.Paths {
					newLines += "      - " + p + "\n"
				}
				abs := idx + ipIdx
				content = content[:abs] + newLines + content[abs:]
			}
		} else {
			// Build a new named filelog receiver block.
			block := "  " + receiverKey + ":\n    include:\n"
			for _, p := range src.Paths {
				block += "      - " + p + "\n"
			}
			block += "    include_file_path: true\n"
			block += "    include_file_name: false\n"
			block += filelogOperatorsBlock(src.Name)

			content = strings.Replace(content, "\nprocessors:", "\n"+block+"\nprocessors:", 1)
		}

		// Add receiver to the logs pipeline, creating the pipeline if needed.
		if !strings.Contains(content, "    logs:") {
			logsPipeline := "    logs:\n" +
				"      receivers: [" + receiverKey + "]\n" +
				"      processors: [memory_limiter, resourcedetection, batch]\n" +
				"      exporters: [zharp]\n"
			content = strings.Replace(content, "    metrics:", logsPipeline+"    metrics:", 1)
		} else {
			re := regexp.MustCompile(`([ \t]+logs:[ \t]*\r?\n[ \t]+receivers:[ \t]*\[)([^\]]*)\]`)
			content = re.ReplaceAllStringFunc(content, func(m string) string {
				sub := re.FindStringSubmatch(m)
				if len(sub) < 3 {
					return m
				}
				existing := strings.TrimSpace(sub[2])
				for _, r := range strings.Split(existing, ",") {
					if strings.TrimSpace(r) == receiverKey {
						return m
					}
				}
				if existing == "" {
					return sub[1] + receiverKey + "]"
				}
				return sub[1] + existing + ", " + receiverKey + "]"
			})
		}
	}

	return os.WriteFile(configFile, []byte(content), 0o644)
}

// appendEnvVars adds or updates KEY=VALUE pairs in the env file.
func appendEnvVars(envFile string, vars map[string]string) error {
	existing := ""
	if data, err := os.ReadFile(envFile); err == nil {
		existing = string(data)
	}
	for k, v := range vars {
		line := k + "=" + v
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `=.*$`)
		if re.MatchString(existing) {
			existing = re.ReplaceAllString(existing, line)
		} else {
			if existing != "" && !strings.HasSuffix(existing, "\n") {
				existing += "\n"
			}
			existing += line + "\n"
		}
	}
	return os.WriteFile(envFile, []byte(existing), 0o600)
}

// normalizeAddArg maps user-facing type names to internal types.
func normalizeAddArg(arg string) string {
	switch strings.ToLower(arg) {
	case "postgres", "postgresql", "pg":
		return "pg"
	case "mysql", "mariadb":
		return "mysql"
	case "redis":
		return "redis"
	case "mongo", "mongodb":
		return "mongo"
	case "docker":
		return "docker"
	case "logs", "log", "custom", "custom_log":
		return "custom_log"
	case "nginx":
		return "nginx_log"
	case "apache", "httpd":
		return "apache_log"
	case "syslog", "system":
		return "syslog"
	case "iis":
		return "iis_log"
	}
	return ""
}

func printAddUsage() {
	fmt.Println()
	fmt.Println("  Usage: zharp-collector add <type> [flags]")
	fmt.Println()
	fmt.Println("  Types:")
	fmt.Println("    postgres  --host HOST --port PORT --user USER --password PASS --db DB [--name NAME]")
	fmt.Println("    mysql     --host HOST --port PORT --user USER --password PASS [--name NAME]")
	fmt.Println("    redis     --endpoint HOST:PORT [--password PASS] [--name NAME]")
	fmt.Println("    mongo     --endpoint HOST:PORT --user USER --password PASS [--name NAME]")
	fmt.Println("    docker")
	fmt.Println("    nginx     [--service NAME] [--access-log PATH] [--error-log PATH]")
	fmt.Println("    apache    [--service NAME] [--access-log PATH]")
	fmt.Println("    syslog    [--service NAME]")
	fmt.Println("    iis       [--service NAME] [--log-folder PATH]")
	fmt.Println("    logs      --path PATH [--path PATH ...] [--service NAME]")
	fmt.Println()
	fmt.Println("  Service name:")
	fmt.Println("    --service sets the name shown in the Zharp dashboard.")
	fmt.Println("    For 'logs', if omitted it is detected from file content or filename.")
	fmt.Println()
	fmt.Println("  Several databases of one engine:")
	fmt.Println("    --name gives a database a name of its own, so a second (third, ...) PostgreSQL,")
	fmt.Println("    MySQL, Redis or MongoDB server is reported separately. The first one needs no")
	fmt.Println("    name; each further one needs a different name.")
	fmt.Println()
	fmt.Println("  Examples:")
	fmt.Println(`    sudo zharp-collector add postgres --host localhost --user zharp_monitor --password secret --db myapp`)
	fmt.Println(`    sudo zharp-collector add postgres --name orders --host 10.0.0.12 --user zharp_monitor --password secret --db orders`)
	fmt.Println(`    sudo zharp-collector add docker`)
	fmt.Println(`    sudo zharp-collector add logs --path /var/log/myapp/app.log --service myapp`)
	fmt.Println(`    sudo zharp-collector add nginx --service frontend`)
	fmt.Println()
}

func printAddTypeUsage(svcType string) {
	switch svcType {
	case "pg":
		fmt.Fprintln(os.Stderr, "  Usage: zharp-collector add postgres --host HOST --port PORT --user USER --password PASS --db DB [--name NAME]")
	case "mysql":
		fmt.Fprintln(os.Stderr, "  Usage: zharp-collector add mysql --host HOST --port PORT --user USER --password PASS [--name NAME]")
	case "redis":
		fmt.Fprintln(os.Stderr, "  Usage: zharp-collector add redis --endpoint HOST:PORT [--password PASS] [--name NAME]")
	case "mongo":
		fmt.Fprintln(os.Stderr, "  Usage: zharp-collector add mongo --endpoint HOST:PORT --user USER --password PASS [--name NAME]")
	case "custom_log":
		fmt.Fprintln(os.Stderr, "  Usage: zharp-collector add logs --path /path/to/file.log [--service myapp]")
	}
}
