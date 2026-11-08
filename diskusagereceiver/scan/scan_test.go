package scan

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBiggestFoldersAndTheirChildren(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "var", "lib", "postgresql", "base.dat"), 900_000)
	write(t, filepath.Join(root, "var", "log", "syslog"), 200_000)
	write(t, filepath.Join(root, "home", "app", "logs", "out.log"), 300_000)
	write(t, filepath.Join(root, "tmp", "x"), 1_000)
	write(t, filepath.Join(root, "top.txt"), 50_000) // a file at the root counts toward the drive only

	res, err := Run(Options{Depth: 2, Top: 2, Children: 1, Timeout: time.Minute, Mounts: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Mounts[0]
	if !m.Complete || m.Files != 5 {
		t.Fatalf("complete %v, files %d", m.Complete, m.Files)
	}
	if len(m.Folders) != 2 {
		t.Fatalf("Top is 2: %+v", m.Folders)
	}
	first := m.Folders[0]
	if filepath.Base(first.Path) != "var" || first.Bytes < 1_100_000 {
		t.Fatalf("var is biggest with both its folders: %+v", first)
	}
	if len(first.Children) != 1 || filepath.Base(first.Children[0].Path) != "lib" {
		t.Fatalf("var's biggest child is lib: %+v", first.Children)
	}
	if filepath.Base(m.Folders[1].Path) != "home" {
		t.Fatalf("home is second: %+v", m.Folders[1])
	}
	if m.Bytes < 1_451_000 {
		t.Fatalf("the drive counts every file: %d", m.Bytes)
	}
}

func TestStopsAtTheTimeLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1500; i++ {
		write(t, filepath.Join(root, "many", time.Duration(i).String()), 10)
	}
	res, err := Run(Options{Depth: 2, Top: 5, Timeout: time.Nanosecond, Mounts: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mounts[0].Complete {
		t.Fatal("a scan past its time limit is marked incomplete")
	}
}
