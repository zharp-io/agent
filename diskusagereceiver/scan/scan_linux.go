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

// drives are the mount points of real drives, each device once (a bind mount isn't a drive).
func drives() ([]string, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byDevice := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !realFileSystems[fields[2]] {
			continue
		}
		mount := unescapeMount(fields[1])
		if skippedMount(mount) {
			continue
		}
		// A single file bind-mounted from a drive (Docker's /etc/hosts) isn't a drive.
		if info, err := os.Stat(mount); err != nil || !info.IsDir() {
			continue
		}
		device := fields[0]
		if current, ok := byDevice[device]; !ok || len(mount) < len(current) {
			byDevice[device] = mount
		}
	}
	mounts := make([]string, 0, len(byDevice))
	for _, m := range byDevice {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts) // "/" first
	return mounts, sc.Err()
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
