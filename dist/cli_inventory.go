package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Inventory statuses, as the dashboard shows them.
const (
	invCollecting = "collecting"  // data is flowing
	invNeedsSetup = "needs_setup" // found on this host, but it needs something only a person can give (a password)
)

// InventoryItem is one thing the collector found on this host and what it is doing about it.
// The exporter reads the file these are written to and sends it with every heartbeat, so the
// dashboard can show what a host is collecting and what is waiting on the person who installed it.
type InventoryItem struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// installInventoryFile is where the inventory lives, next to the config.
func installInventoryFile() string {
	return filepath.Join(installConfigDir(), "inventory.json")
}

func readInventory() []InventoryItem {
	data, err := os.ReadFile(installInventoryFile())
	if err != nil {
		return nil
	}
	var items []InventoryItem
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	return items
}

func writeInventory(items []InventoryItem) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(installInventoryFile(), data, 0o644)
}

// markInventory records that a source is now set up — `add` calls it so a database that was
// waiting for a password shows as collecting on the next heartbeat.
func markInventory(item InventoryItem) {
	items := readInventory()
	for i := range items {
		if items[i].Type == item.Type {
			items[i] = item
			_ = writeInventory(items)
			return
		}
	}
	_ = writeInventory(append(items, item))
}

// inventoryFor describes a source that `add` just configured.
func inventoryFor(svcType string) InventoryItem {
	labels := map[string]string{
		"pg":          "PostgreSQL",
		"mysql":       "MySQL / MariaDB",
		"redis":       "Redis",
		"mongo":       "MongoDB",
		"docker":      "Docker",
		"nginx_log":   "nginx logs",
		"apache_log":  "Apache logs",
		"syslog":      "System logs",
		"iis_log":     "IIS logs",
		"custom_log":  "Log files",
		"docker_logs": "Docker container logs",
		"journald":    "Service logs (journal)",
		"pm2":         "PM2 app logs",
	}
	label := labels[svcType]
	if label == "" {
		label = svcType
	}
	return InventoryItem{Type: svcType, Label: label, Status: invCollecting}
}
