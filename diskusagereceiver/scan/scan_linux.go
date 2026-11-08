//go:build linux

package scan

import (
	"bufio"
	"io/fs"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// realFileSystems are the file systems drives use; everything else in /proc/mounts (proc, tmpfs,
// overlay, cgroup…) is virtual or a container's layer, counted on the drive that holds it.
var realFileSystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"jfs": true, "reiserfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
}

// skippedMounts are mount points that belong to the system or to container runtimes.
var skippedMounts = []string{"/boot/efi", "/snap/", "/var/lib/docker/", "/var/lib/kubelet/", "/var/lib/containers/", "/run/", "/dev/", "/sys/", "/proc/"}

// drives are the mount points of real drives, each device once: where the whole drive is mounted.
// A bind mount of one of its folders (a volume mounted at /mnt/HC_Volume_1 whose postgresql
// folder is also mounted at /var/lib/postgresql) isn't the drive, however short its path.
//
// In a container (root, the host's file system, is /hostfs), the drives are the host's: from its
// init's mount table (<root>/proc/1/mountinfo), each kept only if the same drive is reachable
// under root, so a host mount the container can't see isn't scanned as the folder beneath it.
func drives(root string) ([]string, error) {
	table := "/proc/self/mountinfo"
	if root != "" {
		table = root + "/proc/1/mountinfo"
	}
	data, err := os.ReadFile(table)
	if err != nil {
		return nil, err
	}
	return drivesFrom(string(data), func(mount, device string) bool {
		path := mount
		if root != "" {
			path = root + mount
		}
		// A single file bind-mounted from a drive (Docker's /etc/hosts) isn't a drive.
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return false
		}
		if root == "" {
			return true
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		return ok && deviceNumber(uint64(st.Dev)) == device
	}), nil
}

// deviceNumber is a stat device as mountinfo writes it, "major:minor".
func deviceNumber(dev uint64) string {
	major := (dev>>8)&0xfff | (dev>>32)&^0xfff
	minor := dev&0xff | (dev>>12)&^0xff
	return strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10)
}

// drivesFrom picks the drives out of /proc/self/mountinfo's lines:
//
//	36 35 98:0 /postgresql /var/lib/postgresql rw,noatime - ext4 /dev/sdb rw
//
// (fields: id, parent, major:minor, the folder of the drive mounted, where, options, optional
// fields up to "-", then the file system type and the source). Per device, a mount of the drive's
// own root wins over a mount of one of its folders; among equals, the shortest path.
func drivesFrom(mountinfo string, usable func(mount, device string) bool) []string {
	type pick struct {
		mount   string
		wholeFS bool
	}
	byDevice := map[string]pick{}
	sc := bufio.NewScanner(strings.NewReader(mountinfo))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 5 || sep < 0 || sep+1 >= len(fields) || !realFileSystems[fields[sep+1]] {
			continue
		}
		device, root, mount := fields[2], unescapeMount(fields[3]), unescapeMount(fields[4])
		if skippedMount(mount) || !usable(mount, device) {
			continue
		}
		candidate := pick{mount: mount, wholeFS: root == "/"}
		current, ok := byDevice[device]
		if !ok || (candidate.wholeFS && !current.wholeFS) ||
			(candidate.wholeFS == current.wholeFS && len(candidate.mount) < len(current.mount)) {
			byDevice[device] = candidate
		}
	}
	mounts := make([]string, 0, len(byDevice))
	for _, p := range byDevice {
		mounts = append(mounts, p.mount)
	}
	sort.Strings(mounts) // "/" first
	return mounts
}

func skippedMount(mount string) bool {
	for _, p := range skippedMounts {
		if mount == strings.TrimSuffix(p, "/") || strings.HasPrefix(mount, p) {
			return true
		}
	}
	return false
}

// unescapeMount undoes /proc/mounts' octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func deviceOf(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// diskBytes is the space a file takes on disk (its blocks), as du counts: a sparse database file
// takes less than its size, a tiny one a whole block.
func diskBytes(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}

var (
	userNames   = map[uint32]string{}
	userNamesMu sync.Mutex
)

func ownerOf(info fs.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	userNamesMu.Lock()
	defer userNamesMu.Unlock()
	if name, ok := userNames[st.Uid]; ok {
		return name
	}
	name := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	userNames[st.Uid] = name
	return name
}

// lowerPriority makes the scan yield to everything else: the lowest CPU priority, and the idle
// I/O class (disk access only when nothing else wants it). Both only lower this process.
func lowerPriority() {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
	const ioprioWhoProcess, ioprioClassIdle, ioprioClassShift = 1, 3, 13
	_, _, _ = syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift)
}

// canReadEverything reports root, or the read-only helper's CAP_DAC_READ_SEARCH (bit 2) among
// this process's effective capabilities.
func canReadEverything() bool {
	if os.Geteuid() == 0 {
		return true
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if hex, ok := strings.CutPrefix(line, "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
			return err == nil && caps&(1<<2) != 0
		}
	}
	return false
}
