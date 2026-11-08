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
	folders := 0
	for _, f := range m.Folders {
		if !f.File {
			folders++
		}
	}
	if folders != 2 {
		t.Fatalf("Top is 2 folders (big files at the root are listed beside them): %+v", m.Folders)
	}
	first := m.Folders[0]
	if filepath.Base(first.Path) != "var" || first.Bytes < 1_100_000 {
		t.Fatalf("var is biggest with both its folders: %+v", first)
	}
	var subfolders []Folder
	for _, c := range first.Children {
		if !c.File {
			subfolders = append(subfolders, c)
		}
	}
	if len(subfolders) != 1 || filepath.Base(subfolders[0].Path) != "lib" {
		t.Fatalf("var's biggest child (Children is 1) is lib: %+v", first.Children)
	}
	if filepath.Base(m.Folders[1].Path) != "home" {
		t.Fatalf("home is second: %+v", m.Folders[1])
	}
	if m.Bytes < 1_451_000 {
		t.Fatalf("the drive counts every file: %d", m.Bytes)
	}
}

func TestFollowsTheSpaceDownToTheFolderAndFileHoldingIt(t *testing.T) {
	root := t.TempDir()
	// /home/zharp holds 10 MB, nearly all in one log file four levels down.
	write(t, filepath.Join(root, "home", "zharp", ".pm2", "logs", "api-out.log"), 9_000_000)
	write(t, filepath.Join(root, "home", "zharp", "app", "dist", "main.js"), 500_000)
	write(t, filepath.Join(root, "home", "zharp", "notes.txt"), 10_000) // under 1%: in its folder's total only
	write(t, filepath.Join(root, "swapfile"), 2_000_000)                // a big file at the drive's root

	res, err := Run(Options{Top: 12, Children: 4, Timeout: time.Minute, Mounts: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	folders := res.Mounts[0].Folders
	find := func(list []Folder, base string) *Folder {
		for i := range list {
			if filepath.Base(filepath.FromSlash(list[i].Path)) == base {
				return &list[i]
			}
		}
		t.Fatalf("%s not among %+v", base, list)
		return nil
	}
	if swap := find(folders, "swapfile"); !swap.File {
		t.Fatal("the swap file is reported as a file at the top")
	}
	zharp := find(find(folders, "home").Children, "zharp")
	logs := find(find(zharp.Children, ".pm2").Children, "logs")
	if log := find(logs.Children, "api-out.log"); !log.File || log.Bytes < 9_000_000 {
		t.Fatalf("the log file is under its folder: %+v", log)
	}
	for _, f := range zharp.Children {
		if filepath.Base(filepath.FromSlash(f.Path)) == "notes.txt" {
			t.Fatal("a file under 1% of the drive isn't listed")
		}
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
