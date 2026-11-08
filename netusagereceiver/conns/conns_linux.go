//go:build linux

package conns

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	sockDiagByFamily = 20
	inetDiagInfo     = 2 // the tcp_info attribute
	tcpListen        = 10
	tcpTimeWait      = 6
	tcpSynRecv       = 3

	// The closed-connection announcements: SKNLGRP_INET_TCP_DESTROY (1) and
	// SKNLGRP_INET6_TCP_DESTROY (3), as a bind mask.
	destroyGroups = 1<<(1-1) | 1<<(3-1)

	// tcp_info's tcpi_bytes_acked and tcpi_bytes_received (Linux 4.1 and up).
	bytesAckedOffset    = 120
	bytesReceivedOffset = 128
)

// Supported reports whether this platform can measure connections.
func Supported() bool { return true }

// Dump reads every TCP socket of this network namespace with its byte counters. Any user can.
func Dump() ([]Socket, error) {
	var all []Socket
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		sockets, err := dump(family)
		if err != nil {
			if family == unix.AF_INET6 && errors.Is(err, syscall.ENOENT) {
				continue // IPv6 is off
			}
			return nil, err
		}
		all = append(all, sockets...)
	}
	return all, nil
}

func dump(family uint8) ([]Socket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	// nlmsghdr, then inet_diag_req_v2: family, protocol, extensions, pad, states, sockid.
	req := make([]byte, 16+56)
	binary.LittleEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.LittleEndian.PutUint16(req[4:6], sockDiagByFamily)
	binary.LittleEndian.PutUint16(req[6:8], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	req[16] = family
	req[17] = unix.IPPROTO_TCP
	req[18] = 1 << (inetDiagInfo - 1)
	binary.LittleEndian.PutUint32(req[20:24], ^uint32(1<<tcpTimeWait|1<<tcpSynRecv))
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}

	var out []Socket
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				return out, nil
			case unix.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					if code := int32(binary.LittleEndian.Uint32(m.Data[:4])); code != 0 {
						return out, syscall.Errno(-code)
					}
				}
				return out, nil
			}
			if s, ok := parse(m.Data); ok {
				out = append(out, s)
			}
		}
	}
}

// parse reads an inet_diag_msg and its tcp_info.
func parse(b []byte) (Socket, bool) {
	if len(b) < 72 {
		return Socket{}, false
	}
	family, state := b[0], b[1]
	var s Socket
	s.Listen = state == tcpListen
	localPort := binary.BigEndian.Uint16(b[4:6])
	remotePort := binary.BigEndian.Uint16(b[6:8])
	var local, remote netip.Addr
	if family == unix.AF_INET {
		local = netip.AddrFrom4([4]byte(b[8:12]))
		remote = netip.AddrFrom4([4]byte(b[24:28]))
	} else {
		local = netip.AddrFrom16([16]byte(b[8:24])).Unmap()
		remote = netip.AddrFrom16([16]byte(b[24:40])).Unmap()
	}
	s.Local = netip.AddrPortFrom(local, localPort)
	s.Remote = netip.AddrPortFrom(remote, remotePort)
	s.Cookie = binary.LittleEndian.Uint64(b[44:52])
	s.UID = binary.LittleEndian.Uint32(b[64:68])
	s.Inode = binary.LittleEndian.Uint32(b[68:72])
	attrs := b[72:]
	for len(attrs) >= 4 {
		l := int(binary.LittleEndian.Uint16(attrs[0:2]))
		if l < 4 || l > len(attrs) {
			break
		}
		if binary.LittleEndian.Uint16(attrs[2:4]) == inetDiagInfo && l-4 >= bytesReceivedOffset+8 {
			info := attrs[4:l]
			s.Sent = binary.LittleEndian.Uint64(info[bytesAckedOffset:])
			s.Received = binary.LittleEndian.Uint64(info[bytesReceivedOffset:])
		}
		attrs = attrs[min((l+3)&^3, len(attrs)):]
	}
	return s, true
}

// Watch calls closed with each connection as it closes, with its final counters, until ctx ends.
// Any user can listen.
func Watch(ctx context.Context, closed func(Socket)) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: destroyGroups}); err != nil {
		return err
	}
	// Wake every second to notice ctx ending.
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1})
	buf := make([]byte, 1<<16)
	for ctx.Err() == nil {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			// EAGAIN: the second passed; ENOBUFS: a burst outran the buffer, those few are lost.
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) || errors.Is(err, unix.ENOBUFS) {
				continue
			}
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			continue
		}
		for _, m := range msgs {
			if m.Header.Type != sockDiagByFamily {
				continue
			}
			if s, ok := parse(m.Data); ok && !s.Listen {
				closed(s)
			}
		}
	}
	return nil
}

// Processes lists the processes holding sockets and which ones. Without root or CAP_SYS_PTRACE
// only this user's own processes can be read; Privileged says which it was.
func Processes() Owners {
	out := Owners{Privileged: privileged()}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue
		}
		var inodes []uint32
		for _, f := range fds {
			link, err := os.Readlink(dir + "/fd/" + f.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32); err == nil {
				inodes = append(inodes, uint32(n))
			}
		}
		if len(inodes) == 0 {
			continue
		}
		comm, _ := os.ReadFile(dir + "/comm")
		cmdline, _ := os.ReadFile(dir + "/cmdline")
		owner := ""
		var st syscall.Stat_t
		if syscall.Stat(dir, &st) == nil {
			owner = UserName(st.Uid)
		}
		out.Processes = append(out.Processes, Process{
			PID:         pid,
			Name:        strings.TrimSpace(string(comm)),
			CommandLine: TrimCommandLine(strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")),
			Owner:       owner,
			Inodes:      inodes,
		})
	}
	return out
}

var userNames = map[uint32]string{}

// UserName is a user's name, or its number when it has none.
func UserName(uid uint32) string {
	if name, ok := userNames[uid]; ok {
		return name
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	userNames[uid] = name
	return name
}

// privileged reports CAP_SYS_PTRACE (bit 19) among the effective capabilities: what reading other
// users' processes' open sockets takes. Root has it, except where it was dropped (a container's
// root, by default), so it's the capability that's checked, not the user.
func privileged() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if hex, ok := strings.CutPrefix(line, "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
			return err == nil && caps&(1<<19) != 0
		}
	}
	return false
}
