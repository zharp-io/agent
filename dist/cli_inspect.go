package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zharp-io/agent/diskusagereceiver/scan"
)

// inspectHelperName is the read-only helper: a copy of this binary, installed at
// /usr/local/lib/zharp-collector/zharp-inspect with CAP_DAC_READ_SEARCH so it can list every
// folder (names and sizes, never contents) for Servers & Containers' disk usage.
const inspectHelperName = "zharp-inspect"

// isInspectHelper reports whether this process is the helper copy. Decided by the file it runs
// from, which a caller can't change the way it can change argv[0].
func isInspectHelper() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	return base == inspectHelperName
}

// runInspectHelper is all the helper copy does: measure disk usage. Anything else (running the
// collector, any other command) is refused, so its read-only access can't be put to other use.
func runInspectHelper() {
	if len(os.Args) >= 3 && os.Args[1] == "inspect" && os.Args[2] == "disk" {
		cliInspectDisk(os.Args[3:])
		return
	}
	fmt.Fprintln(os.Stderr, "zharp-inspect only measures disk usage: zharp-inspect inspect disk [--json] [folder…]")
	os.Exit(2)
}

// cliInspect is `zharp-collector inspect …`.
func cliInspect() {
	if len(os.Args) >= 3 && os.Args[2] == "disk" {
		cliInspectDisk(os.Args[3:])
		return
	}
	fmt.Fprintln(os.Stderr, "Usage: zharp-collector inspect disk [--json] [--top N] [--children N] [--timeout 4m] [folder…]")
	os.Exit(2)
}

// cliInspectDisk measures what takes each drive's space, like du: the biggest folders, two levels
// down, and the space each user owns. --json is what the zharp_disk_usage receiver reads.
func cliInspectDisk(args []string) {
	fs := flag.NewFlagSet("inspect disk", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON (what the collector reads)")
	top := fs.Int("top", 12, "top-level folders per drive")
	children := fs.Int("children", 4, "sub-folders under each")
	timeout := fs.Duration("timeout", 4*time.Minute, "stop after this long and report what was measured")
	_ = fs.Parse(args)

	res, err := scan.Run(scan.Options{Depth: 2, Top: *top, Children: *children, Timeout: *timeout, Mounts: fs.Args()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "  [!] "+err.Error())
		os.Exit(1)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	printDiskUsage(res)
}

func printDiskUsage(res scan.Result) {
	for _, m := range res.Mounts {
		fmt.Printf("\n%s  %s in %d files", m.Mount, humanBytes(m.Bytes), m.Files)
		if !m.Complete {
			fmt.Print("  (stopped at the time limit)")
		}
		fmt.Println()
		for _, f := range m.Folders {
			fmt.Printf("  %9s  %s%s\n", humanBytes(f.Bytes), f.Path, ownedBy(f.Owner))
			for _, c := range f.Children {
				fmt.Printf("  %9s    %s%s\n", humanBytes(c.Bytes), c.Path, ownedBy(c.Owner))
			}
		}
		if len(m.Owners) > 0 {
			fmt.Println("  By owner:")
			for _, o := range m.Owners {
				if o.Bytes > 0 {
					fmt.Printf("  %9s  %s\n", humanBytes(o.Bytes), o.Owner)
				}
			}
		}
		if m.Unreadable > 0 && res.Privileged {
			fmt.Printf("  %d folders went away during the scan.\n", m.Unreadable)
		} else if m.Unreadable > 0 {
			fmt.Printf("  %d folders couldn't be read: run as root to count them.\n", m.Unreadable)
		}
	}
}

func ownedBy(owner string) string {
	if owner == "" {
		return ""
	}
	return "  (" + owner + ")"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
