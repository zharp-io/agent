package zharpexporter

import (
	"encoding/json"
	"os"
)

// maxInventoryBytes bounds what a heartbeat carries. The installer writes a few hundred bytes;
// anything far past that is not a file it wrote.
const maxInventoryBytes = 64 * 1024

// readInventory returns the services `zharp-collector install` recorded, as-is, for the
// heartbeat. A missing or unreadable file means an older install, and the heartbeat goes out
// without the list rather than failing.
func readInventory(path string) json.RawMessage {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > maxInventoryBytes {
		return nil
	}
	var items []map[string]any
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	return json.RawMessage(data)
}
