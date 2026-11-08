// Package scan measures what takes a server's disk space, the way `du` and `ncdu` would: the
// biggest folders on each drive, followed down to where the space actually is (any folder or file
// holding at least 1% of the drive's used space, up to six levels down), and the space each user
// owns. It reads directory listings and file sizes only, never file contents.
//
// The zharp_disk_usage receiver runs it hourly through `zharp-collector inspect disk` (or the
// read-only helper copy, zharp-inspect, which can list every folder). People can run that command
// themselves too.
package scan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options bound a scan.
type Options struct {
	// Depth is how many folder levels below a drive's root are measured on their own.
	Depth int
	// Top is how many of a drive's top-level folders are reported.
	Top int
	// Children is how many sub-folders are reported under each reported folder, of those holding
	// at least 1% of the drive's used space.
	Children int
	// Timeout bounds a whole scan; what was measured by then is reported, marked incomplete.
	Timeout time.Duration
	// Mounts restricts the scan to these mount points; empty means every real drive.
	Mounts []string
}

// Folder is a folder and the space under it, or, with File, one big file.
type Folder struct {
	Path     string   `json:"path"`
	Bytes    int64    `json:"bytes"`
	Owner    string   `json:"owner,omitempty"`
	File     bool     `json:"file,omitempty"`
	Children []Folder `json:"children,omitempty"`
}

const (
	// biggestFilesKept is how many of a drive's biggest files are remembered during a walk; those
	// holding at least 1% of the drive are reported, under the folder they're in.
	biggestFilesKept = 30
	// foldersReported bounds a drive's reported folders and files, however its space is spread.
	foldersReported = 200
)

// Owner is the space a user's files take on a drive.
type Owner struct {
	Owner string `json:"owner"`
	Bytes int64  `json:"bytes"`
}

// Mount is one drive's scan.
type Mount struct {
	Mount   string   `json:"mount"`
	Bytes   int64    `json:"bytes"`
	Files   int64    `json:"files"`
	Folders []Folder `json:"folders"`
	Owners  []Owner  `json:"owners"`
	// Unreadable counts folders that couldn't be listed (another user's, without the helper).
	Unreadable int64 `json:"unreadable"`
	// Complete is false when the scan hit its time limit.
	Complete bool    `json:"complete"`
	Seconds  float64 `json:"seconds"`
}

// Result is a whole scan.
type Result struct {
	// Privileged says whether every folder could be listed (the read-only helper, or root).
	Privileged bool    `json:"privileged"`
	Mounts     []Mount `json:"mounts"`
}

// Run scans every drive in turn, at the lowest CPU and disk priority this process can set.
func Run(opts Options) (Result, error) {
	if opts.Depth < 1 {
		opts.Depth = 6
	}
	if opts.Top < 1 {
		opts.Top = 12
	}
	if opts.Children < 0 {
		opts.Children = 0
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 4 * time.Minute
	}
	lowerPriority()

	mounts := opts.Mounts
	if len(mounts) == 0 {
		var err error
		if mounts, err = drives(); err != nil {
			return Result{}, err
		}
	}
	if len(mounts) == 0 {
		return Result{}, errors.New("no drives to scan")
	}

	res := Result{Privileged: canReadEverything()}
	deadline := time.Now().Add(opts.Timeout)
	for _, mount := range mounts {
		res.Mounts = append(res.Mounts, scanMount(mount, opts, deadline))
	}
	return res, nil
}

// skippedUnderRoot are kernel and runtime file systems: never on a drive, often enormous to walk.
var skippedUnderRoot = []string{"/proc", "/sys", "/dev", "/run"}

func scanMount(root string, opts Options, deadline time.Time) Mount {
	started := time.Now()
	m := Mount{Mount: root, Complete: true}
	rootDev, haveDev := deviceOf(root)
	sizes := map[string]int64{}  // folder path (depth 1..Depth) → bytes under it
	owners := map[string]int64{} // user → bytes
	var files []Folder           // the biggest files, at most biggestFilesKept
	seen := int64(0)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() || errors.Is(err, fs.ErrPermission) {
				m.Unreadable++
				return filepath.SkipDir
			}
			return nil // a file that vanished mid-scan
		}
		seen++
		if seen%1000 == 0 {
			if time.Now().After(deadline) {
				m.Complete = false
				return filepath.SkipAll
			}
			if seen%5000 == 0 {
				time.Sleep(5 * time.Millisecond) // keep the disk free for real work
			}
		}
		if d.IsDir() && path != root {
			if root == "/" && isSkippedRoot(path) {
				return filepath.SkipDir
			}
			// Another drive mounted inside this one has its own scan.
			if haveDev {
				if dev, ok := deviceOf(path); ok && dev != rootDev {
					return filepath.SkipDir
				}
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		size := diskBytes(info)
		owner := ownerOf(info)
		if !d.IsDir() {
			m.Files++
			files = keepBiggest(files, Folder{Path: filepath.ToSlash(path), Bytes: size, Owner: owner, File: true})
		}
		m.Bytes += size
		owners[owner] += size
		if rel, ok := relativeParts(root, path, d.IsDir()); ok {
			for depth := 1; depth <= opts.Depth && depth <= len(rel); depth++ {
				sizes[joinUnder(root, rel[:depth])] += size
			}
		}
		return nil
	})
	if errors.Is(err, filepath.SkipAll) {
		m.Complete = false
	}

	m.Folders = topFolders(root, sizes, files, m.Bytes, opts)
	m.Owners = topOwners(owners)
	m.Seconds = time.Since(started).Seconds()
	return m
}

func isSkippedRoot(path string) bool {
	for _, p := range skippedUnderRoot {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// relativeParts are the folders between root and path: for a file, its parent's; for a folder,
// its own.
func relativeParts(root, path string, isDir bool) ([]string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return nil, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if !isDir {
		parts = parts[:len(parts)-1]
	}
	return parts, len(parts) > 0
}

func joinUnder(root string, parts []string) string {
	return filepath.ToSlash(filepath.Join(append([]string{root}, parts...)...))
}

// keepBiggest adds a file to the biggest ones, dropping the smallest past biggestFilesKept.
func keepBiggest(files []Folder, f Folder) []Folder {
	if len(files) < biggestFilesKept {
		return append(files, f)
	}
	smallest := 0
	for i := range files {
		if files[i].Bytes < files[smallest].Bytes {
			smallest = i
		}
	}
	if f.Bytes > files[smallest].Bytes {
		files[smallest] = f
	}
	return files
}

func parentOf(path string) string {
	return filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
}

// topFolders is a drive's tree of where its space is: its biggest top-level folders, and under
// each reported folder its biggest sub-folders holding at least 1% of the drive's used space,
// down to Depth, with the biggest files (also 1% and up) in the deepest reported folder holding
// them. Everything smaller is in its folder's total.
func topFolders(root string, sizes map[string]int64, files []Folder, total int64, opts Options) []Folder {
	children := map[string][]Folder{}
	for path, bytes := range sizes {
		children[parentOf(path)] = append(children[parentOf(path)], Folder{Path: path, Bytes: bytes})
	}
	least := total / 100
	budget := foldersReported
	var build func(parent string, depth, limit int) []Folder
	build = func(parent string, depth, limit int) []Folder {
		kids := children[parent]
		sort.Slice(kids, func(a, b int) bool { return kids[a].Bytes > kids[b].Bytes })
		var out []Folder
		for _, k := range kids {
			if len(out) >= limit || budget <= 0 || (depth > 1 && k.Bytes < least) {
				break
			}
			budget--
			k.Owner = ownerOfPath(k.Path)
			if depth < opts.Depth {
				k.Children = build(k.Path, depth+1, opts.Children)
			}
			out = append(out, k)
		}
		return out
	}
	tree := build(filepath.ToSlash(root), 1, opts.Top)

	sort.Slice(files, func(a, b int) bool { return files[a].Bytes > files[b].Bytes })
	for _, f := range files {
		if f.Bytes < least || f.Bytes <= 0 || budget <= 0 {
			break
		}
		budget--
		tree = placeFile(tree, f)
	}
	return tree
}

// placeFile puts a file in the deepest reported folder holding it, or among the drive's top-level
// entries (a /swapfile) when none does.
func placeFile(folders []Folder, f Folder) []Folder {
	for i := range folders {
		if !folders[i].File && strings.HasPrefix(f.Path, folders[i].Path+"/") {
			folders[i].Children = placeFile(folders[i].Children, f)
			return folders
		}
	}
	folders = append(folders, f)
	sort.SliceStable(folders, func(a, b int) bool { return folders[a].Bytes > folders[b].Bytes })
	return folders
}

func topOwners(owners map[string]int64) []Owner {
	out := make([]Owner, 0, len(owners))
	for owner, bytes := range owners {
		out = append(out, Owner{Owner: owner, Bytes: bytes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

func ownerOfPath(path string) string {
	info, err := os.Lstat(filepath.FromSlash(path))
	if err != nil {
		return ""
	}
	return ownerOf(info)
}
