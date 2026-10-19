package zharpexporter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadInventory(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "inventory.json")
	_ = os.WriteFile(good, []byte(`[{"type":"pg","label":"PostgreSQL","status":"needs_setup"}]`), 0o644)
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"not":"a list"}`), 0o644)

	if got := readInventory(good); string(got) == "" {
		t.Fatal("a valid inventory should be sent")
	}
	for _, path := range []string{"", bad, filepath.Join(dir, "missing.json")} {
		if got := readInventory(path); got != nil {
			t.Errorf("%q: got %s, want nothing", path, got)
		}
	}
}

func TestBaseURL(t *testing.T) {
	if got := (&Config{}).baseURL(); got != defaultEndpoint {
		t.Errorf("default: %s", got)
	}
	if got := (&Config{Endpoint: "https://zharp.example.com/api/v1/"}).baseURL(); got != "https://zharp.example.com/api/v1" {
		t.Errorf("custom: %s", got)
	}
}
