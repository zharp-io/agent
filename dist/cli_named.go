package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Several databases of one engine on one host.
//
// The default receiver of each engine ("postgresql", "mysql", "redis", "mongodb") feeds the
// main metrics pipeline, and the backend files its points under the engine's name. Nothing in a
// receiver's own metrics tells two PostgreSQL servers apart, so `add <engine> --name NAME`
// gives the extra database
//
//   - a named receiver ("postgresql/orders"), with its own password variable (PG_PASSWORD_ORDERS),
//   - its own pipeline ("metrics/postgresql_orders"), ending in
//   - its own instance of the Zharp exporter ("zharp/postgresql_orders") whose `database`
//     setting stamps every point with zharp.database=postgresql/orders.
//
// The backend registers each distinct zharp.database as its own database of that agent. Hosts
// set up before this keep their single, unnamed receiver and report exactly as they did.

var databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)

// checkDatabaseName refuses names that would not survive as a component id or env variable.
func checkDatabaseName(name string) error {
	if name == "" || databaseNamePattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("--name must be 1-40 letters, digits, '-' or '_' (got %q)", name)
}

// namedIDs is the receiver id and password variable for a database: the engine's defaults for
// an unnamed one, suffixed with the name otherwise.
func namedIDs(receiver, envVar, name string) (id, env string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return receiver, envVar
	}
	return receiver + "/" + name, envVar + "_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// databaseNameFlag is the --name given to a database type, lower-cased; "" for other types.
func databaseNameFlag(svcType string, args []string) string {
	switch svcType {
	case "pg", "mysql", "redis", "mongo":
	default:
		return ""
	}
	for i, arg := range args {
		for _, prefix := range []string{"--name", "-name"} {
			if arg == prefix && i+1 < len(args) {
				return strings.ToLower(strings.TrimSpace(args[i+1]))
			}
			if strings.HasPrefix(arg, prefix+"=") {
				return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, prefix+"=")))
			}
		}
	}
	return ""
}

// receiverIDs splits a comma-separated receiver list.
func receiverIDs(receivers string) []string {
	var ids []string
	for _, r := range strings.Split(receivers, ",") {
		if r = strings.TrimSpace(r); r != "" {
			ids = append(ids, r)
		}
	}
	return ids
}

// receiverConfigured reports whether the config already holds the first of these receivers.
func receiverConfigured(configFile, receivers string) bool {
	ids := receiverIDs(receivers)
	data, err := os.ReadFile(configFile)
	if err != nil || len(ids) == 0 {
		return false
	}
	return strings.Contains(string(data), "\n  "+ids[0]+":")
}

// addNamedDatabase returns the config with a named database's receivers, exporter instance and
// pipeline added. A database already present under that name keeps its settings (existed=true);
// only its password variable is then updated by the caller.
func addNamedDatabase(content, receivers, block string) (out string, existed bool, err error) {
	ids := receiverIDs(receivers)
	if len(ids) == 0 {
		return content, false, fmt.Errorf("no receiver to add")
	}
	primary := ids[0]
	if strings.Contains(content, "\n  "+primary+":") {
		return content, true, nil
	}
	pipeline := strings.ReplaceAll(primary, "/", "_")
	exporterID := "zharp/" + pipeline

	// 1. The receivers, beside the others.
	const processorsMarker = "\nprocessors:"
	if !strings.Contains(content, processorsMarker) {
		return content, false, fmt.Errorf("could not find processors section in config")
	}
	block = strings.TrimRight(block, "\n") + "\n"
	content = strings.Replace(content, processorsMarker, "\n"+block+processorsMarker, 1)

	// 2. An exporter instance with the main one's key and endpoint, stamping the database.
	const mainExporter = "\n  zharp:\n"
	start := strings.Index(content, mainExporter)
	if start < 0 {
		return content, false, fmt.Errorf("could not find the zharp exporter in config")
	}
	bodyStart := start + len(mainExporter)
	end := bodyStart
	var settings []string
	for end < len(content) {
		lineEnd := strings.IndexByte(content[end:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content) - end
		}
		line := content[end : end+lineEnd]
		if !strings.HasPrefix(line, "    ") {
			break
		}
		// The heartbeat (and the inventory it carries) stays with the main exporter.
		if !strings.HasPrefix(strings.TrimSpace(line), "inventory_file:") {
			settings = append(settings, line)
		}
		end += lineEnd + 1
	}
	if end > len(content) {
		end = len(content)
	}
	exporter := "  " + exporterID + ":\n" + strings.Join(settings, "\n")
	if len(settings) > 0 {
		exporter += "\n"
	}
	exporter += fmt.Sprintf("    database: %q\n", primary)
	content = content[:end] + exporter + content[end:]

	// 3. The pipeline that joins them.
	const pipelinesMarker = "\n  pipelines:\n"
	if !strings.Contains(content, pipelinesMarker) {
		return content, false, fmt.Errorf("could not find service pipelines in config")
	}
	pipelineBlock := fmt.Sprintf("    metrics/%s:\n      receivers: [%s]\n      processors: [memory_limiter, resourcedetection, batch]\n      exporters: [%s]\n",
		pipeline, strings.Join(ids, ", "), exporterID)
	content = strings.Replace(content, pipelinesMarker, pipelinesMarker+pipelineBlock, 1)

	return content, false, nil
}

// applyNamedDatabase writes a named database to the config and its password to the env file.
func applyNamedDatabase(configFile, envFile, receivers, block string, envVars map[string]string, name string) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not read config: %v\n", err)
		os.Exit(1)
	}
	content, existed, err := addNamedDatabase(string(data), receivers, block)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not update config: %v\n", err)
		os.Exit(1)
	}
	if existed {
		uiWarn(fmt.Sprintf("A database named %q is already configured; its settings were kept.", name))
	} else if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not update config: %v\n", err)
		os.Exit(1)
	}
	if len(envVars) > 0 {
		if err := appendEnvVars(envFile, envVars); err != nil {
			uiWarn("Could not update env file: " + err.Error())
		}
	}
	uiOK("Config updated → " + configFile)
}
