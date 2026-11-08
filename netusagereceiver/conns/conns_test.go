package conns

import (
	"net/netip"
	"testing"
)

var (
	api      = Program{Name: "node", CommandLine: "node /srv/api/main.js", Owner: "zharp"}
	postgres = Program{Name: "postgres", CommandLine: "/usr/lib/postgresql/16/bin/postgres -D", Owner: "postgres"}
	nginx    = Program{Name: "nginx", CommandLine: "nginx: worker process", Owner: "www-data"}
)

func sock(cookie uint64, local, remote string, inode uint32, sent, received uint64) Socket {
	return Socket{Cookie: cookie, Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort(remote), Inode: inode, Sent: sent, Received: received}
}

func listener(local string, inode uint32) Socket {
	return Socket{Listen: true, Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort("0.0.0.0:0"), Inode: inode}
}

func owners(byInode map[uint32]Program) func(Socket) Program {
	return func(s Socket) Program { return byInode[s.Inode] }
}

func find(flows []Flow, p Program, role, peer string, port uint16) *Flow {
	for i := range flows {
		f := &flows[i]
		if f.Program == p && f.Role == role && f.Peer == peer && f.Port == port {
			return f
		}
	}
	return nil
}

func TestTheFirstSampleIsTheBaselineAndLaterOnesCountWhatChanged(t *testing.T) {
	m := NewMeter()
	who := owners(map[uint32]Program{1: api, 2: postgres, 10: postgres})
	// An old connection with a lifetime of traffic: not this period's.
	m.Sample([]Socket{
		listener("0.0.0.0:5432", 10),
		sock(100, "10.0.0.5:40000", "140.82.112.3:443", 1, 5_000_000, 9_000_000),
	}, who)
	m.Sample([]Socket{
		listener("0.0.0.0:5432", 10),
		sock(100, "10.0.0.5:40000", "140.82.112.3:443", 1, 5_001_000, 9_004_000),
		// New since the last sample: counts in full; to a listening port, it's the server's.
		sock(101, "10.0.0.5:5432", "10.0.0.9:51000", 2, 300, 700),
	}, who)
	flows := m.Take(10, 5)

	client := find(flows, api, "client", "140.82.112.3", 443)
	if client == nil || client.Sent != 1000 || client.Received != 4000 || client.Scope != "public" {
		t.Fatalf("api to github = %+v", client)
	}
	server := find(flows, postgres, "server", "", 5432)
	if server == nil || server.Sent != 300 || server.Received != 700 || server.Scope != "private" {
		t.Fatalf("postgres serving = %+v", server)
	}
	if again := m.Take(10, 5); len(again) != 0 {
		t.Fatalf("a taken period is empty after: %+v", again)
	}
}

func TestClosedConnectionsCountTheirLastTraffic(t *testing.T) {
	m := NewMeter()
	who := owners(map[uint32]Program{1: api, 10: nginx})
	m.Sample([]Socket{listener("0.0.0.0:443", 10), sock(100, "10.0.0.5:40000", "52.1.1.1:443", 1, 100, 100)}, who)
	m.Sample([]Socket{listener("0.0.0.0:443", 10), sock(100, "10.0.0.5:40000", "52.1.1.1:443", 1, 150, 300)}, who)
	// Closed after more traffic: only what came after the last sample is added.
	m.Closed(Socket{Cookie: 100, Local: netip.MustParseAddrPort("10.0.0.5:40000"), Remote: netip.MustParseAddrPort("52.1.1.1:443"), Sent: 400, Received: 1300})
	// Opened and closed between samples, to nginx's port: nginx's, in full.
	m.Closed(Socket{Cookie: 200, Local: netip.MustParseAddrPort("10.0.0.5:443"), Remote: netip.MustParseAddrPort("81.2.3.4:61000"), Sent: 9000, Received: 500})
	// Opened and closed between samples, out to somewhere: whose isn't known any more.
	m.Closed(Socket{Cookie: 300, Local: netip.MustParseAddrPort("10.0.0.5:41000"), Remote: netip.MustParseAddrPort("8.8.8.8:443"), Sent: 10, Received: 20})
	flows := m.Take(10, 5)

	if f := find(flows, api, "client", "52.1.1.1", 443); f == nil || f.Sent != 300 || f.Received != 1200 {
		t.Fatalf("api = %+v", f)
	}
	if f := find(flows, nginx, "server", "", 443); f == nil || f.Sent != 9000 || f.Received != 500 || f.Scope != "public" {
		t.Fatalf("nginx = %+v", f)
	}
	if f := find(flows, Program{}, "client", "8.8.8.8", 443); f == nil || f.Sent != 10 {
		t.Fatalf("unknown = %+v", f)
	}
}

func TestALateClosingAnnouncementIsNotCountedTwice(t *testing.T) {
	m := NewMeter()
	who := owners(map[uint32]Program{1: api})
	m.Sample([]Socket{sock(100, "10.0.0.5:40000", "52.1.1.1:443", 1, 100, 100)}, who)
	m.Sample([]Socket{sock(100, "10.0.0.5:40000", "52.1.1.1:443", 1, 200, 200)}, who)
	// Gone from the table before its announcement arrives.
	m.Sample(nil, who)
	m.Closed(Socket{Cookie: 100, Local: netip.MustParseAddrPort("10.0.0.5:40000"), Remote: netip.MustParseAddrPort("52.1.1.1:443"), Sent: 250, Received: 200})
	flows := m.Take(10, 5)
	if f := find(flows, api, "client", "52.1.1.1", 443); f == nil || f.Sent != 150 || f.Received != 100 {
		t.Fatalf("api = %+v", f)
	}
	if len(flows) != 1 {
		t.Fatalf("flows = %+v", flows)
	}
}

func TestOnlyTheBusiestProgramsAndPeersAreKept(t *testing.T) {
	m := NewMeter()
	m.Sample(nil, owners(nil))
	who := owners(map[uint32]Program{1: api, 2: postgres, 3: nginx})
	m.Sample([]Socket{
		sock(1, "10.0.0.5:40001", "52.1.1.1:443", 1, 1000, 0),
		sock(2, "10.0.0.5:40002", "52.1.1.2:443", 1, 500, 0),
		sock(3, "10.0.0.5:40003", "52.1.1.3:443", 1, 100, 0),
		sock(4, "10.0.0.5:40004", "127.0.0.1:6379", 1, 50, 0),
		sock(5, "10.0.0.5:40005", "10.0.0.7:5432", 2, 400, 0),
		sock(6, "10.0.0.5:40006", "10.0.0.8:80", 3, 10, 0),
	}, who)
	flows := m.Take(2, 2)

	if f := find(flows, api, "client", "52.1.1.1", 443); f == nil {
		t.Fatal("api's busiest peer is kept")
	}
	if f := find(flows, api, "client", "52.1.1.3", 443); f != nil {
		t.Fatal("api's third peer is added into its rest")
	}
	rest := map[string]uint64{}
	for _, f := range flows {
		if f.Program == api && f.Role == "other" {
			rest[f.Scope] += f.Sent
		}
	}
	if rest["public"] != 100 || rest["loopback"] != 50 || len(rest) != 2 {
		t.Fatalf("api's rest by scope = %v", rest)
	}
	if f := find(flows, postgres, "client", "10.0.0.7", 5432); f == nil {
		t.Fatal("postgres is the second busiest program")
	}
	var other *Flow
	for i := range flows {
		if flows[i].Other {
			other = &flows[i]
		}
	}
	if other == nil || other.Sent != 10 || other.Scope != "private" {
		t.Fatalf("the remaining programs = %+v", other)
	}
}

func TestCommandLinesKeepTheProgramAndTwoArguments(t *testing.T) {
	cases := map[string]string{
		"node /srv/api/dist/main.js --port 3000": "node /srv/api/dist/main.js --port",
		"java -jar app.jar --password hunter2":   "java -jar app.jar",
		"nginx":                                  "nginx",
		`"C:\Program Files\App\app.exe" --x y z`: `"C:\Program Files\App\app.exe" --x y`,
	}
	for in, want := range cases {
		if got := TrimCommandLine(in); got != want {
			t.Errorf("TrimCommandLine(%q) = %q, want %q", in, got, want)
		}
	}
}
