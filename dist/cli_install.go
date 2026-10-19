package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// svcInstallConfig holds everything svcInstall needs.
type svcInstallConfig struct {
	binaryPath string
	configFile string
	envFile    string
	envVars    map[string]string
	hasDocker  bool
}

// cliInstall sets the collector up as a service.
//
// With an API key and --yes (or with no terminal to ask on, as under `curl | sh`) it runs
// unattended: everything detected that needs no password is collected with its defaults, and the
// rest is reported to the dashboard as waiting for setup. Otherwise it runs the guided wizard.
func cliInstall() {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	keyFlag := fs.String("api-key", "", "workspace API key (or ZHARP_API_KEY)")
	endpointFlag := fs.String("endpoint", "", "Zharp API URL, for self-hosted installs (or ZHARP_ENDPOINT)")
	yes := fs.Bool("yes", false, "set up everything that needs no password, without asking")
	fs.BoolVar(yes, "y", false, "shorthand for --yes")
	_ = fs.Parse(os.Args[2:])

	presetKey := firstNonEmpty(*keyFlag, os.Getenv("ZHARP_API_KEY"))
	endpoint := strings.TrimRight(firstNonEmpty(*endpointFlag, os.Getenv("ZHARP_ENDPOINT")), "/")

	// Require root on non-Windows.
	if runtime.GOOS != "windows" {
		if os.Getuid() != 0 {
			fmt.Fprintln(os.Stderr, "  [!] This command must be run as root. Try: sudo zharp-collector install")
			os.Exit(1)
		}
	}

	if presetKey != "" && (*yes || !isTerminal(os.Stdin)) {
		installUnattended(presetKey, endpoint)
		return
	}

	// Step 1: Banner.
	uiBanner()

	// Step 2: Scan.
	uiSection("1 · Scanning this server...")
	detected := detectServices()
	if len(detected) > 0 {
		uiOK("Detected:")
		for _, s := range detected {
			uiDimMsg(fmt.Sprintf("%-22s %s", s.Label, s.Detail))
		}
	} else {
		uiDimMsg("No services auto-detected.")
	}

	// Step 3: API Key.
	uiSection("3 · API Key")
	uiDimMsg("Get yours at: https://zharp.io/settings/api-keys")
	fmt.Println()
	apiKey := presetKey
	for apiKey == "" {
		apiKey = strings.TrimSpace(uiAsk("Paste your API key:"))
		if apiKey == "" {
			uiWarn("API key is required.")
		}
	}
	uiOK("API key saved.")

	// Step 4: Service selection.
	uiSection("4 · What do you want to monitor?")
	uiDimMsg("Host metrics (CPU, memory, disk, network) are always collected.")
	fmt.Println()

	options := allOptions(detected)
	doneTypes := map[string]bool{}

	metricsReceivers := []string{"hostmetrics"}
	var receiverBlocks []string
	var logSources []LogSource
	allEnvVars := map[string]string{
		"ZHARP_API_KEY": apiKey,
	}
	hasDocker := false
	tracing := false
	var configured []InventoryItem

	for {
		// Build current menu.
		type menuEntry struct {
			opt ServiceOption
			num int
		}
		var detectedMenu []menuEntry
		var manualMenu []menuEntry

		n := 0
		for _, opt := range options {
			if doneTypes[opt.Type] {
				continue
			}
			n++
			if opt.Detected {
				detectedMenu = append(detectedMenu, menuEntry{opt, n})
			} else {
				manualMenu = append(manualMenu, menuEntry{opt, n})
			}
		}

		if len(detectedMenu) == 0 && len(manualMenu) == 0 {
			uiOK("All available services configured.")
			break
		}

		if len(detectedMenu) > 0 {
			fmt.Printf("  %sDetected on this server:%s\n", colBold, colReset)
			uiHR()
			for _, e := range detectedMenu {
				fmt.Printf("  %s[%d]%s  %-24s %s%s%s\n",
					colCyan, e.num, colReset,
					e.opt.Label,
					colDim, e.opt.Detail, colReset)
			}
		}

		if len(manualMenu) > 0 {
			if len(detectedMenu) > 0 {
				fmt.Println()
			}
			fmt.Printf("  %sAdd manually:%s\n", colBold, colReset)
			uiHR()
			for _, e := range manualMenu {
				fmt.Printf("  %s[%d]%s  %-24s %s%s%s\n",
					colCyan, e.num, colReset,
					e.opt.Label,
					colDim, e.opt.Detail, colReset)
			}
		}

		fmt.Println()
		pick := strings.TrimSpace(uiAsk("Pick a number to configure, or press Enter to finish:"))
		if pick == "" {
			break
		}

		// Find picked option.
		var selected *ServiceOption
		idx := 0
		fmt.Sscanf(pick, "%d", &idx)
		if idx < 1 || idx > n {
			uiWarn(fmt.Sprintf("Enter a number between 1 and %d, or press Enter to finish.", n))
			fmt.Println()
			continue
		}
		// Find which option has this number.
		cur := 0
		for i := range options {
			if doneTypes[options[i].Type] {
				continue
			}
			cur++
			if cur == idx {
				selected = &options[i]
				break
			}
		}
		if selected == nil {
			continue
		}

		fmt.Println()
		fmt.Printf("  %sConfiguring: %s%s\n", colBold, selected.Label, colReset)
		if selected.Type == "apm" {
			doneTypes[selected.Type] = true
			tracing = true
			uiOK("Applications can send traces to 127.0.0.1:4318 (HTTP) or 127.0.0.1:4317 (gRPC).")
			uiDimMsg("Point your OpenTelemetry SDK at that endpoint — no API key needed, the collector already has one.")

			fmt.Println()
			more := strings.TrimSpace(uiAsk("Monitor another service? [Y/n]:"))
			if strings.EqualFold(more, "n") {
				break
			}
			fmt.Println()
			continue
		}

		receiver, block, sources, ev, isDocker := configureService(selected.Type)
		doneTypes[selected.Type] = true
		if receiver != "" || len(sources) > 0 {
			configured = append(configured, inventoryFor(selected.Type))
		}

		if receiver != "" {
			metricsReceivers = append(metricsReceivers, receiver)
		}
		if block != "" {
			receiverBlocks = append(receiverBlocks, block)
		}
		logSources = append(logSources, sources...)
		for k, v := range ev {
			allEnvVars[k] = v
		}
		if isDocker {
			hasDocker = true
		}

		fmt.Println()
		more := strings.TrimSpace(uiAsk("Monitor another service? [Y/n]:"))
		if strings.EqualFold(more, "n") {
			break
		}
		fmt.Println()
	}

	// Detected but skipped stays visible in the dashboard, so it can be set up later.
	inventory := []InventoryItem{{Type: "host", Label: "Host metrics", Status: invCollecting, Detail: "CPU, memory, disk, network and load"}}
	if tracing {
		inventory = append(inventory, InventoryItem{Type: "apm", Label: "Application traces", Status: invCollecting, Detail: "OpenTelemetry on 127.0.0.1:4317 (gRPC) and :4318 (HTTP)"})
	}
	inventory = append(inventory, configured...)
	for _, d := range detected {
		if !doneTypes[d.Type] {
			inventory = append(inventory, InventoryItem{Type: d.Type, Label: d.Label, Status: invNeedsSetup, Detail: "Skipped during install"})
		}
	}

	finishInstall(buildConfigYAML(metricsReceivers, receiverBlocks, logSources, tracing, endpoint), allEnvVars, hasDocker, inventory, true)
}

// installUnattended is the no-questions install: detect, set up what needs no password, start.
func installUnattended(apiKey, endpoint string) {
	fmt.Println()
	uiInfo("Scanning this server...")
	plan := planAutomatic(detectServices())
	plan.envVars["ZHARP_API_KEY"] = apiKey

	for _, item := range plan.inventory {
		if item.Status == invCollecting {
			uiOK(fmt.Sprintf("%-20s %s", item.Label, item.Detail))
		} else {
			uiWarn(fmt.Sprintf("%-20s %s — finish it from the Zharp dashboard", item.Label, item.Detail))
		}
	}

	finishInstall(buildConfigYAML(plan.metricsReceivers, plan.receiverBlocks, plan.logSources, plan.tracing, endpoint), plan.envVars, plan.hasDocker, plan.inventory, false)
}

// finishInstall writes the config, secrets and inventory, then installs and starts the service.
func finishInstall(yaml string, envVars map[string]string, hasDocker bool, inventory []InventoryItem, guided bool) {
	if guided {
		uiSection("5 · Writing config")
	}

	configDir := installConfigDir()
	configFile := installConfigFile()
	envFile := installEnvFile()

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Cannot create config directory %s: %v\n", configDir, err)
		os.Exit(1)
	}

	if err := os.WriteFile(configFile, []byte(yaml), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Cannot write config: %v\n", err)
		os.Exit(1)
	}
	uiOK("Config → " + configFile)

	// Write env file.
	var envLines []string
	for k, v := range envVars {
		envLines = append(envLines, k+"="+v)
	}
	envContent := strings.Join(envLines, "\n") + "\n"
	envPerm := os.FileMode(0o600)
	if err := os.WriteFile(envFile, []byte(envContent), envPerm); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Cannot write env file: %v\n", err)
		os.Exit(1)
	}
	uiOK("Secrets → " + envFile)

	if err := writeInventory(inventory); err != nil {
		uiWarn("Could not write the inventory: " + err.Error())
	}

	// Step 6: Install service.
	if guided {
		uiSection("6 · Installing service")
	}

	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Cannot determine binary path: %v\n", err)
		os.Exit(1)
	}

	cfg := &svcInstallConfig{
		binaryPath: binaryPath,
		configFile: configFile,
		envFile:    envFile,
		envVars:    envVars,
		hasDocker:  hasDocker,
	}

	if err := svcInstall(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Service install failed: %v\n", err)
		os.Exit(1)
	}

	// Done.
	fmt.Println()
	fmt.Printf("%s  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", colBold, colReset)
	fmt.Printf("%s%s    Done! Zharp Collector is running.%s\n", colGreen, colBold, colReset)
	fmt.Printf("%s  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", colBold, colReset)
	fmt.Println()
	fmt.Printf("  %sConfig:%s  %s\n", colBold, colReset, configFile)
	fmt.Printf("  %sSecrets:%s %s\n", colBold, colReset, envFile)
	fmt.Println()
	printDoneCommands()
	fmt.Println()
	uiDimMsg("This host appears in your Zharp dashboard within a minute.")
	fmt.Println()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// configureService prompts for service-specific config and returns the
// receiver name, YAML block, log sources, env vars, and docker flag.
func configureService(svcType string) (receiver, block string, logSources []LogSource, ev map[string]string, isDocker bool) {
	ev = map[string]string{}

	switch svcType {
	case "nginx_log":
		fmt.Printf("  %snginx — log paths%s\n", colBold, colReset)
		svcName := promptDefault("Service name", "nginx")
		defaultAccess := "/var/log/nginx/access.log"
		if runtime.GOOS == "windows" {
			defaultAccess = `C:\nginx\logs\access.log`
		} else if runtime.GOOS == "darwin" {
			defaultAccess = "/usr/local/var/log/nginx/access.log"
		}
		p := promptDefault("Access log", defaultAccess)
		paths := []string{p}
		defaultError := ""
		if runtime.GOOS == "windows" {
			defaultError = `C:\nginx\logs\error.log`
		} else if runtime.GOOS == "darwin" {
			defaultError = "/usr/local/var/log/nginx/error.log"
		} else {
			defaultError = "/var/log/nginx/error.log"
		}
		e := strings.TrimSpace(uiAsk(fmt.Sprintf("Error log [%s] (blank to skip):", defaultError)))
		if e != "" {
			paths = append(paths, e)
		} else if fileExists(defaultError) {
			paths = append(paths, defaultError)
		}
		logSources = []LogSource{{Name: sanitizeServiceName(svcName), Paths: paths}}
		uiOK("nginx logs configured.")

	case "apache_log":
		fmt.Printf("  %sApache — log path%s\n", colBold, colReset)
		svcName := promptDefault("Service name", "apache")
		defaultLog := "/var/log/apache2/access.log"
		if fileExists("/var/log/httpd/access_log") {
			defaultLog = "/var/log/httpd/access_log"
		}
		p := promptDefault("Access log", defaultLog)
		logSources = []LogSource{{Name: sanitizeServiceName(svcName), Paths: []string{p}}}
		uiOK("Apache logs configured.")

	case "syslog":
		svcName := promptDefault("Service name", "syslog")
		var paths []string
		if fileExists("/var/log/syslog") {
			paths = append(paths, "/var/log/syslog")
		}
		if fileExists("/var/log/messages") {
			paths = append(paths, "/var/log/messages")
		}
		if fileExists("/var/log/auth.log") {
			paths = append(paths, "/var/log/auth.log")
		}
		if len(paths) > 0 {
			logSources = []LogSource{{Name: sanitizeServiceName(svcName), Paths: paths}}
		}
		uiOK("System logs configured.")

	case "iis_log":
		svcName := promptDefault("Service name", "iis")
		defaultFolder := `C:\inetpub\logs\LogFiles\`
		p := promptDefault("IIS log folder", defaultFolder)
		if !strings.HasSuffix(p, `\`) {
			p += `\`
		}
		logSources = []LogSource{{Name: sanitizeServiceName(svcName), Paths: []string{p + `**\*.log`}}}
		uiOK("IIS logs configured.")

	case "custom_log":
		fmt.Printf("  %sCustom log source%s\n", colBold, colReset)
		uiDimMsg("The service name is shown in the Zharp dashboard to group these logs.")
		svcName := ""
		for svcName == "" {
			svcName = strings.TrimSpace(uiAsk("Service name (e.g. myapp):"))
			if svcName == "" {
				uiWarn("Service name is required.")
			}
		}
		svcName = sanitizeServiceName(svcName)
		fmt.Println()
		uiDimMsg("Glob patterns OK: /var/log/myapp/*.log")
		uiDimMsg("Press Enter on a blank line when done.")
		fmt.Println()
		var paths []string
		for {
			p := strings.TrimSpace(uiAsk("Log path (blank to finish):"))
			if p == "" {
				break
			}
			paths = append(paths, p)
			uiOK("Added: " + p)
		}
		if len(paths) > 0 {
			logSources = []LogSource{{Name: svcName, Paths: paths}}
		}

	case "pg":
		fmt.Printf("  %sPostgreSQL%s\n", colBold, colReset)
		h := promptDefault("Host", "localhost")
		pt := promptDefault("Port", "5432")
		u := promptDefault("User", "zharp_monitor")
		pass := uiReadPassword("Password:")
		db := promptDefault("Database", "postgres")
		receiver = "postgresql"
		block = fmt.Sprintf(`
  postgresql:
    endpoint: %s:%s
    username: %s
    password: "${env:PG_PASSWORD}"
    databases:
      - %s
    collection_interval: 30s
    tls:
      insecure: true
`, h, pt, u, db)
		ev["PG_PASSWORD"] = pass
		uiOK("PostgreSQL configured.")
		fmt.Println()
		uiWarn("Required — run once as superuser:")
		uiDimMsg(fmt.Sprintf("CREATE USER %s WITH PASSWORD 'your_password';", u))
		uiDimMsg(fmt.Sprintf("GRANT pg_monitor TO %s;", u))

	case "mysql":
		fmt.Printf("  %sMySQL / MariaDB%s\n", colBold, colReset)
		h := promptDefault("Host", "localhost")
		pt := promptDefault("Port", "3306")
		u := promptDefault("User", "zharp_monitor")
		pass := uiReadPassword("Password:")
		receiver, block = mysqlReceivers(h, pt, u)
		ev["MYSQL_PASSWORD"] = pass
		uiOK("MySQL configured.")
		fmt.Println()
		uiWarn("Required — run once as root:")
		uiDimMsg(fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED BY 'your_password';", u))
		uiDimMsg(fmt.Sprintf("GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO '%s'@'localhost';", u))
		uiDimMsg("FLUSH PRIVILEGES;")

	case "redis":
		fmt.Printf("  %sRedis%s\n", colBold, colReset)
		ep := promptDefault("Endpoint", "localhost:6379")
		pass := uiReadPassword("Password (blank if none):")
		receiver = "redis"
		if pass != "" {
			block = fmt.Sprintf(`
  redis:
    endpoint: %s
    password: "${env:REDIS_PASSWORD}"
    collection_interval: 30s
`, ep)
			ev["REDIS_PASSWORD"] = pass
		} else {
			block = fmt.Sprintf(`
  redis:
    endpoint: %s
    collection_interval: 30s
`, ep)
		}
		uiOK("Redis configured.")

	case "mongo":
		fmt.Printf("  %sMongoDB%s\n", colBold, colReset)
		ep := promptDefault("Endpoint", "localhost:27017")
		u := promptDefault("User", "zharp_monitor")
		pass := uiReadPassword("Password:")
		receiver = "mongodb"
		block = fmt.Sprintf(`
  mongodb:
    hosts:
      - endpoint: %s
    username: %s
    password: "${env:MONGO_PASSWORD}"
    collection_interval: 30s
    tls:
      insecure: true
`, ep, u)
		ev["MONGO_PASSWORD"] = pass
		uiOK("MongoDB configured.")
		fmt.Println()
		uiWarn("Required — run once in mongosh as admin:")
		uiDimMsg(fmt.Sprintf("db.createUser({ user: '%s', pwd: 'your_password',", u))
		uiDimMsg("  roles: [{ role: 'clusterMonitor', db: 'admin' }] })")

	case "docker":
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
		isDocker = true
		uiOK("Docker container metrics configured.")
	}

	return receiver, block, logSources, ev, isDocker
}
