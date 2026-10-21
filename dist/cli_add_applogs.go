package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// cliAddAppLogs is `zharp-collector add docker-logs|journal|pm2`, for hosts installed before
// application logs were collected automatically (or with --no-app-logs).
func cliAddAppLogs(svcType, configFile string) {
	r, items, ok := configureAppLogs(svcType)
	for _, it := range items {
		markInventory(it)
	}
	if !ok {
		os.Exit(1)
	}
	if svcType == "pm2" && len(items) == 1 && items[0].Status == invCollecting {
		removeInventory("pm2:setup") // everything is readable now
	}
	if svcType == "docker_logs" {
		grantDockerAccess()
	}
	if err := prepareStateDir(); err != nil {
		uiWarn("Could not create " + installStateDir() + ": " + err.Error())
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		os.Exit(1)
	}
	content := addLogReceiver(string(data), r)
	if svcType == "journald" {
		content = dropJournalOverlap(content, fileExists("/var/log/kern.log"))
	}
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not update config: %v\n", err)
		os.Exit(1)
	}
	uiOK("Config updated → " + configFile)

	uiInfo("Restarting service to apply changes...")
	if err := svcRestart(); err != nil {
		uiWarn("Restart failed: " + err.Error())
		uiDimMsg("Restart manually: zharp-collector restart")
	} else {
		uiOK("Done. Logs will appear in your Zharp dashboard within a minute.")
	}
	fmt.Println()
}

// addLogReceiver puts r's block in the config (replacing an older one of the same id, so a
// second `add pm2` picks up new directories), adds it to the logs pipeline, and turns on the
// storage extension when r needs it.
func addLogReceiver(content string, r LogReceiver) string {
	return ensureLogRules(addLogReceiverContent(content, r))
}

// addLogReceiverContent is addLogReceiver without the log rules processor (see
// addLogSourcesToContent).
func addLogReceiverContent(content string, r LogReceiver) string {
	content = removeReceiverBlock(content, r.ID)
	content = strings.Replace(content, "\nprocessors:", "\n"+strings.TrimRight(r.Block, "\n")+"\n\nprocessors:", 1)
	content = ensureInLogsPipeline(content, r.ID)
	if r.Storage {
		content = ensureStorageExtension(content)
	}
	return content
}

// removeReceiverBlock deletes "  <id>:" and everything indented under it.
func removeReceiverBlock(content, id string) string {
	header := "\n  " + id + ":\n"
	start := strings.Index(content, header)
	if start < 0 {
		return content
	}
	end := start + len(header)
	for end < len(content) {
		nl := strings.IndexByte(content[end:], '\n')
		line := content[end:]
		if nl >= 0 {
			line = content[end : end+nl]
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "   ") {
			break
		}
		if nl < 0 {
			end = len(content)
			break
		}
		end += nl + 1
	}
	return content[:start+1] + content[end:]
}

var logsPipelineRe = regexp.MustCompile(`([ \t]+logs:[ \t]*\r?\n[ \t]+receivers:[ \t]*\[)([^\]]*)\]`)

// ensureInLogsPipeline adds id to the logs pipeline, creating the pipeline if there is none.
func ensureInLogsPipeline(content, id string) string {
	if !logsPipelineRe.MatchString(content) {
		logs := "    logs:\n" +
			"      receivers: [" + id + "]\n" +
			"      processors: [memory_limiter, resourcedetection, batch]\n" +
			"      exporters: [zharp]\n"
		return strings.Replace(content, "    metrics:", logs+"    metrics:", 1)
	}
	return logsPipelineRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := logsPipelineRe.FindStringSubmatch(m)
		list := splitList(sub[2])
		if contains(list, id) {
			return m
		}
		return sub[1] + strings.Join(append(list, id), ", ") + "]"
	})
}

// removeFromLogsPipeline takes id out of the logs pipeline, and the pipeline out when empty.
func removeFromLogsPipeline(content, id string) string {
	content = logsPipelineRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := logsPipelineRe.FindStringSubmatch(m)
		var keep []string
		for _, r := range splitList(sub[2]) {
			if r != id {
				keep = append(keep, r)
			}
		}
		return sub[1] + strings.Join(keep, ", ") + "]"
	})
	return regexp.MustCompile(`    logs:\n      receivers: \[\]\n(      .*\n)*`).ReplaceAllString(content, "")
}

func splitList(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

var serviceExtensionsRe = regexp.MustCompile(`(\n  extensions:[ \t]*\[)([^\]]*)\]`)

// ensureStorageExtension declares file_storage and enables it in the service.
func ensureStorageExtension(content string) string {
	if !strings.Contains(content, "\n  "+storageExtension+":\n") {
		content = "\n" + content
		if strings.Contains(content, "\nextensions:\n") {
			content = strings.Replace(content, "\nextensions:\n", "\nextensions:\n"+storageExtensionBlock(), 1)
		} else {
			content = "\nextensions:\n" + storageExtensionBlock() + content
		}
		content = content[1:]
	}
	if serviceExtensionsRe.MatchString(content) {
		return serviceExtensionsRe.ReplaceAllStringFunc(content, func(m string) string {
			sub := serviceExtensionsRe.FindStringSubmatch(m)
			list := splitList(sub[2])
			if contains(list, storageExtension) {
				return m
			}
			return sub[1] + strings.Join(append(list, storageExtension), ", ") + "]"
		})
	}
	return strings.Replace(content, "\nservice:\n", "\nservice:\n  extensions: ["+storageExtension+"]\n", 1)
}

// dropJournalOverlap is withoutJournalOverlap for a config already on disk: the tailed
// /var/log/syslog and /var/log/messages go (kern.log takes their place when it exists), and a
// file receiver left with nothing to tail is removed.
func dropJournalOverlap(content string, kernLog bool) string {
	lines := strings.SplitAfter(content, "\n")
	var out []string
	replaced := false
	for _, l := range lines {
		path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")), `'"`)
		if strings.HasPrefix(l, "      - ") && syslogFilesCoveredByJournal[path] {
			if kernLog && !replaced && !strings.Contains(content, "/var/log/kern.log") {
				out = append(out, "      - /var/log/kern.log\n")
				replaced = true
			}
			continue
		}
		out = append(out, l)
	}
	content = strings.Join(out, "")

	// A filelog receiver whose include list is now empty.
	empty := regexp.MustCompile(`\n  (filelog/[^:\s]+):\n    include:\n    [a-z_]`)
	for {
		m := empty.FindStringSubmatch(content)
		if m == nil {
			return content
		}
		content = removeReceiverBlock(content, m[1])
		content = removeFromLogsPipeline(content, m[1])
	}
}

// removeInventory drops an item from the inventory.
func removeInventory(typ string) {
	items := readInventory()
	var keep []InventoryItem
	for _, it := range items {
		if it.Type != typ {
			keep = append(keep, it)
		}
	}
	if len(keep) != len(items) {
		_ = writeInventory(keep)
	}
}
