// Package conns measures each program's network traffic, the way `ss` and `nethogs` see it: the
// kernel's table of TCP connections (sock_diag) gives every connection's byte counters, which any
// user can read, and the processes holding each connection say whose it is, which only root or a
// process with CAP_SYS_PTRACE can read for other users' programs (the read-only helper).
//
// Connections that open and close between two samples are caught too: the kernel announces each
// closed connection with its final counters.
package conns

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Socket is one TCP connection or listener.
type Socket struct {
	// Cookie identifies the socket for as long as it exists.
	Cookie uint64
	Listen bool
	Local  netip.AddrPort
	Remote netip.AddrPort
	// UID owns it; Inode is what a process's open files point at. Both are 0 for a closed one.
	UID   uint32
	Inode uint32
	// Sent counts the bytes the other side acknowledged; Received, the bytes that arrived.
	Sent     uint64
	Received uint64
}

// Process is a process holding sockets, as the helper reports it.
type Process struct {
	PID         int      `json:"pid"`
	Name        string   `json:"name"`
	CommandLine string   `json:"commandLine"`
	Owner       string   `json:"owner"`
	Inodes      []uint32 `json:"inodes"`
}

// Owners is the helper's answer: who holds which socket.
type Owners struct {
	// Privileged is false when only the helper's own user's processes could be read.
	Privileged bool      `json:"privileged"`
	Processes  []Process `json:"processes"`
}

// Program is whose traffic it is. Name is empty when the connection's process wasn't seen (Owner
// may still say whose it is).
type Program struct {
	Name        string
	CommandLine string
	Owner       string
}

// Flow is traffic of one program with one peer.
type Flow struct {
	Program Program
	// Role is "client" (it connected out), "server" (clients connected to it) or "other" (the
	// remaining peers of a program, or the remaining programs, added up).
	Role string
	// Peer is the address a client talks to; empty for a server's clients.
	Peer string
	// Port is the port a client connects to, or the one a server listens on.
	Port uint16
	// Scope is the peer's network: "loopback" (this machine), "private" or "public".
	Scope    string
	Sent     uint64
	Received uint64
	// Other marks the programs beyond the ones kept, added up.
	Other bool
}

// The program and its first two arguments, a "quoted path with spaces" counting as one: what the
// processes pipeline keeps of a command line (transform/processes), so a program's traffic and its
// CPU and memory are named alike. The rest of a command line can hold passwords and tokens.
var commandLineHead = regexp.MustCompile(`^("[^"]*"|[^ ]+)( +("[^"]*"|[^ ]+))?( +("[^"]*"|[^ ]+))?.*$`)

// TrimCommandLine keeps a command line's program and its first two arguments.
func TrimCommandLine(commandLine string) string {
	commandLine = strings.TrimSpace(commandLine)
	return commandLineHead.ReplaceAllString(commandLine, "$1$2$4")
}

type flowKey struct {
	program Program
	role    string
	peer    string
	port    uint16
	scope   string
}

type tracked struct {
	sock    Socket
	key     flowKey
	missing int
}

// Meter adds up connections' traffic into flows between samples. Safe for concurrent use: the
// closed-connection watch calls Closed while the sampler calls Sample.
type Meter struct {
	mu        sync.Mutex
	started   bool
	sockets   map[uint64]*tracked
	listeners map[uint16]Program
	flows     map[flowKey]*Flow
}

// NewMeter starts an empty meter: its first Sample is the baseline, not traffic.
func NewMeter() *Meter {
	return &Meter{sockets: map[uint64]*tracked{}, listeners: map[uint16]Program{}, flows: map[flowKey]*Flow{}}
}

// Sample takes the socket table; programOf says whose a socket is. The traffic since the previous
// sample is added to each connection's flow; a connection that's new since then counts in full.
func (m *Meter) Sample(sockets []Socket, programOf func(Socket) Program) {
	m.mu.Lock()
	defer m.mu.Unlock()

	listeners := map[uint16]Program{}
	for _, s := range sockets {
		if s.Listen {
			p := programOf(s)
			if current, ok := listeners[s.Local.Port()]; !ok || current.Name == "" {
				listeners[s.Local.Port()] = p
			}
		}
	}
	m.listeners = listeners

	present := make(map[uint64]bool, len(sockets))
	for _, s := range sockets {
		if s.Listen {
			continue
		}
		present[s.Cookie] = true
		key := m.keyOf(s, programOf(s))
		if t, ok := m.sockets[s.Cookie]; ok {
			// A process seen later names a connection first seen without one.
			if t.key.program.Name != "" {
				key.program = t.key.program
			}
			m.add(key, delta(s.Sent, t.sock.Sent), delta(s.Received, t.sock.Received))
			t.sock, t.key, t.missing = s, key, 0
			continue
		}
		if m.started {
			m.add(key, s.Sent, s.Received)
		}
		m.sockets[s.Cookie] = &tracked{sock: s, key: key}
	}
	// A connection that's gone waits a sample for its closing announcement, which may come late.
	for cookie, t := range m.sockets {
		if !present[cookie] {
			if t.missing++; t.missing > 2 {
				delete(m.sockets, cookie)
			}
		}
	}
	m.started = true
}

// Closed takes a closed connection's final counters.
func (m *Meter) Closed(s Socket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	if t, ok := m.sockets[s.Cookie]; ok {
		m.add(t.key, delta(s.Sent, t.sock.Sent), delta(s.Received, t.sock.Received))
		delete(m.sockets, s.Cookie)
		return
	}
	// Opened and closed between samples: a server's connection belongs to whoever listens on its
	// port; a client's process is gone, so it's counted without one.
	m.add(m.keyOf(s, Program{}), s.Sent, s.Received)
}

func (m *Meter) keyOf(s Socket, p Program) flowKey {
	scope := scopeOf(s.Remote.Addr())
	if listener, ok := m.listeners[s.Local.Port()]; ok {
		if p.Name == "" {
			p = listener
		}
		return flowKey{program: p, role: "server", port: s.Local.Port(), scope: scope}
	}
	return flowKey{program: p, role: "client", peer: s.Remote.Addr().Unmap().String(), port: s.Remote.Port(), scope: scope}
}

func (m *Meter) add(key flowKey, sent, received uint64) {
	if sent == 0 && received == 0 {
		return
	}
	f, ok := m.flows[key]
	if !ok {
		f = &Flow{Program: key.program, Role: key.role, Peer: key.peer, Port: key.port, Scope: key.scope}
		m.flows[key] = f
	}
	f.Sent += sent
	f.Received += received
}

// Take returns the period's flows and starts the next period: the programs with the most traffic
// (at most programs of them), each with its busiest peers (at most peers) and the rest of its
// traffic added up; the remaining programs added up into one Other flow per scope.
func (m *Meter) Take(programs, peers int) (flows []Flow) {
	m.mu.Lock()
	all := m.flows
	m.flows = map[flowKey]*Flow{}
	m.mu.Unlock()

	byProgram := map[Program][]*Flow{}
	totals := map[Program]uint64{}
	for _, f := range all {
		byProgram[f.Program] = append(byProgram[f.Program], f)
		totals[f.Program] += f.Sent + f.Received
	}
	order := make([]Program, 0, len(byProgram))
	for p := range byProgram {
		order = append(order, p)
	}
	sort.Slice(order, func(i, j int) bool {
		if totals[order[i]] != totals[order[j]] {
			return totals[order[i]] > totals[order[j]]
		}
		return order[i].Name < order[j].Name
	})

	rest := map[string]*Flow{}
	for i, p := range order {
		list := byProgram[p]
		if i >= programs {
			for _, f := range list {
				o, ok := rest[f.Scope]
				if !ok {
					o = &Flow{Role: "other", Scope: f.Scope, Other: true}
					rest[f.Scope] = o
				}
				o.Sent += f.Sent
				o.Received += f.Received
			}
			continue
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Sent+list[a].Received > list[b].Sent+list[b].Received })
		others := map[string]*Flow{}
		for j, f := range list {
			if j < peers {
				flows = append(flows, *f)
				continue
			}
			o, ok := others[f.Scope]
			if !ok {
				o = &Flow{Program: p, Role: "other", Scope: f.Scope}
				others[f.Scope] = o
			}
			o.Sent += f.Sent
			o.Received += f.Received
		}
		for _, scope := range []string{"loopback", "private", "public"} {
			if o, ok := others[scope]; ok {
				flows = append(flows, *o)
			}
		}
	}
	for _, scope := range []string{"loopback", "private", "public"} {
		if o, ok := rest[scope]; ok {
			flows = append(flows, *o)
		}
	}
	return flows
}

func delta(now, before uint64) uint64 {
	if now < before {
		return 0
	}
	return now - before
}

func scopeOf(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback() || a.IsUnspecified():
		return "loopback"
	case a.IsPrivate() || a.IsLinkLocalUnicast() || isSharedAddressSpace(a):
		return "private"
	default:
		return "public"
	}
}

// isSharedAddressSpace is 100.64.0.0/10, carrier-grade NAT and Tailscale.
func isSharedAddressSpace(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	b := a.As4()
	return b[0] == 100 && b[1]&0xc0 == 64
}
