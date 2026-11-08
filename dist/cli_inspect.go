package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zharp-io/agent/diskusagereceiver/scan"
	"github.com/zharp-io/agent/netusagereceiver/conns"
)

// inspectHelperName is the read-only helper: a copy of this binary, installed at
// /usr/local/lib/zharp-collector/zharp-inspect with CAP_DAC_READ_SEARCH, so it can list every
// folder (names and sizes, never contents) for Servers & Containers' disk usage, and
// CAP_SYS_PTRACE, so it can see which process holds which network connection (the sockets in
// /proc/<pid>/fd) for its network usage.
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

// runInspectHelper is all the helper copy does: measure disk usage and list which process holds
// which socket. Anything else (running the collector, any other command) is refused, so its
// read-only access can't be put to other use.
func runInspectHelper() {
	if len(os.Args) >= 3 && os.Args[1] == "inspect" {
		switch os.Args[2] {
		case "disk":
			cliInspectDisk(os.Args[3:])
			return
		case "sockets":
			cliInspectSockets(os.Args[3:])
			return
		}
	}
	fmt.Fprintln(os.Stderr, "zharp-inspect only measures disk usage and lists sockets' processes: zharp-inspect inspect disk|sockets")
	os.Exit(2)
}

// cliInspect is `zharp-collector inspect …`.
func cliInspect() {
	if len(os.Args) >= 3 {
		switch os.Args[2] {
		case "disk":
			cliInspectDisk(os.Args[3:])
			return
		case "network":
			cliInspectNetwork(os.Args[3:])
			return
		case "sockets":
			cliInspectSockets(os.Args[3:])
			return
		}
	}
	fmt.Fprintln(os.Stderr, "Usage: zharp-collector inspect disk [--json] [--top N] [--children N] [--timeout 4m] [folder…]")
	fmt.Fprintln(os.Stderr, "       zharp-collector inspect network [--seconds 10]")
	os.Exit(2)
}

// cliInspectSockets lists the processes holding sockets as JSON, what the zharp_network_usage
// receiver reads: once, or with --serve once per line read from stdin, so the receiver keeps one
// helper running rather than starting one every few seconds.
func cliInspectSockets(args []string) {
	fs := flag.NewFlagSet("inspect sockets", flag.ExitOnError)
	serve := fs.Bool("serve", false, "answer once per line read from stdin")
	_ = fs.Parse(args)
	out := json.NewEncoder(os.Stdout)
	if !*serve {
		_ = out.Encode(conns.Processes())
		return
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		if err := out.Encode(conns.Processes()); err != nil {
			return
		}
	}
}

// cliInspectNetwork measures each program's traffic for a few seconds, like nethogs: what the
// Network breakdown shows, on the spot. As root it names every program.
func cliInspectNetwork(args []string) {
	fs := flag.NewFlagSet("inspect network", flag.ExitOnError)
	seconds := fs.Int("seconds", 10, "how long to measure")
	_ = fs.Parse(args)
	if !conns.Supported() {
		fmt.Fprintln(os.Stderr, "  [!] measuring connections is only supported on Linux")
		os.Exit(1)
	}

	meter := conns.NewMeter()
	byInode := map[uint32]conns.Program{}
	owners := conns.Processes()
	refresh := func() {
		owners = conns.Processes()
		for _, p := range owners.Processes {
			for _, inode := range p.Inodes {
				byInode[inode] = conns.Program{Name: p.Name, CommandLine: p.CommandLine, Owner: p.Owner}
			}
		}
	}
	programOf := func(s conns.Socket) conns.Program {
		if p, ok := byInode[s.Inode]; ok {
			return p
		}
		if s.Inode == 0 {
			return conns.Program{}
		}
		return conns.Program{Owner: conns.UserName(s.UID)}
	}
	sample := func() bool {
		sockets, err := conns.Dump()
		if err != nil {
			fmt.Fprintln(os.Stderr, "  [!] "+err.Error())
			return false
		}
		refresh()
		meter.Sample(sockets, programOf)
		return true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conns.Watch(ctx, meter.Closed) }()
	if !sample() {
		os.Exit(1)
	}
	fmt.Printf("Measuring for %ds…\n", *seconds)
	for i := 0; i < *seconds; i++ {
		time.Sleep(time.Second)
		if !sample() {
			os.Exit(1)
		}
	}
	printNetworkUsage(meter.Take(15, 5), float64(*seconds), owners.Privileged)
}

func printNetworkUsage(flows []conns.Flow, seconds float64, privileged bool) {
	type program struct {
		name           string
		sent, received uint64
		flows          []conns.Flow
	}
	var order []*program
	byName := map[string]*program{}
	for _, f := range flows {
		name := networkProgramName(f)
		p, ok := byName[name]
		if !ok {
			p = &program{name: name}
			byName[name] = p
			order = append(order, p)
		}
		p.sent += f.Sent
		p.received += f.Received
		p.flows = append(p.flows, f)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].sent+order[i].received > order[j].sent+order[j].received })
	rate := func(n uint64) string { return humanBytes(int64(float64(n)/seconds)) + "/s" }
	fmt.Printf("\n  %12s  %12s  program\n", "sent", "received")
	for _, p := range order {
		fmt.Printf("  %12s  %12s  %s\n", rate(p.sent), rate(p.received), p.name)
		for _, f := range p.flows {
			fmt.Printf("  %12s  %12s    %s\n", rate(f.Sent), rate(f.Received), networkPeer(f))
		}
	}
	if len(order) == 0 {
		fmt.Println("  No TCP traffic in that time.")
	}
	if !privileged {
		fmt.Println("\n  Only your own programs are named: run as root to name every program.")
	}
}

func networkProgramName(f conns.Flow) string {
	switch {
	case f.Other:
		return "other programs"
	case f.Program.Name == "" && f.Program.Owner != "":
		return "programs run by " + f.Program.Owner
	case f.Program.Name == "":
		return "closed before their program was seen"
	}
	name := f.Program.CommandLine
	if name == "" {
		name = f.Program.Name
	}
	if f.Program.Owner != "" {
		name += "  (" + f.Program.Owner + ")"
	}
	return name
}

func networkPeer(f conns.Flow) string {
	where := map[string]string{"loopback": "this machine", "private": "private network", "public": "internet"}[f.Scope]
	switch f.Role {
	case "server":
		return fmt.Sprintf("clients on :%d (%s)", f.Port, where)
	case "client":
		return fmt.Sprintf("%s:%d (%s)", f.Peer, f.Port, where)
	}
	return "everything else (" + where + ")"
}

// cliInspectDisk measures what takes each drive's space, like du and ncdu: the biggest folders,
// followed down to the folders and files holding the space, and the space each user owns. --json
// is what the zharp_disk_usage receiver reads.
func cliInspectDisk(args []string) {
	fs := flag.NewFlagSet("inspect disk", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON (what the collector reads)")
	top := fs.Int("top", 12, "top-level folders per drive")
	children := fs.Int("children", 4, "sub-folders under each, of those holding 1% of the drive or more")
	depth := fs.Int("depth", 6, "folder levels followed down")
	timeout := fs.Duration("timeout", 4*time.Minute, "stop after this long and report what was measured")
	_ = fs.Parse(args)

	res, err := scan.Run(scan.Options{Depth: *depth, Top: *top, Children: *children, Timeout: *timeout, Mounts: fs.Args()})
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
		var tree func(folders []scan.Folder, indent string)
		tree = func(folders []scan.Folder, indent string) {
			for _, f := range folders {
				kind := ""
				if f.File {
					kind = "  [file]"
				}
				fmt.Printf("  %9s  %s%s%s%s\n", humanBytes(f.Bytes), indent, f.Path, kind, ownedBy(f.Owner))
				tree(f.Children, indent+"  ")
			}
		}
		tree(m.Folders, "")
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
