package netusagereceiver

import (
	"testing"
	"time"

	"github.com/zharp-io/agent/netusagereceiver/conns"
)

func TestMetricsAreRatesPerProgramPeerAndDirection(t *testing.T) {
	node := conns.Program{Name: "node", CommandLine: "node /srv/api/main.js", Owner: "zharp"}
	flows := []conns.Flow{
		{Program: node, Role: "client", Peer: "140.82.112.3", Port: 443, Scope: "public", Sent: 6000, Received: 60000},
		{Program: conns.Program{Owner: "www-data"}, Role: "server", Port: 80, Scope: "public", Received: 600},
		{Role: "other", Scope: "private", Sent: 120, Other: true},
	}
	md := Metrics(flows, map[string]string{"140.82.112.3": "lb-140-82-112-3-iad.github.com"}, time.Minute, true, true, time.Unix(1700000000, 0))

	metrics := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if metrics.Len() != 2 || metrics.At(0).Name() != "zharp.network.io" || metrics.At(1).Name() != "zharp.network.scan" {
		t.Fatalf("metrics = %d", metrics.Len())
	}
	points := metrics.At(0).Gauge().DataPoints()
	// Two directions for node, one each for the others (nothing for a direction without bytes).
	if points.Len() != 4 {
		t.Fatalf("points = %d", points.Len())
	}
	first := points.At(0)
	if first.DoubleValue() != 100 {
		t.Fatalf("node's transmit rate = %v, want 100 B/s", first.DoubleValue())
	}
	a := first.Attributes().AsRaw()
	if a["process.executable.name"] != "node" || a["network.peer.name"] != "lb-140-82-112-3-iad.github.com" ||
		a["network.peer.port"] != int64(443) || a["network.io.direction"] != "transmit" || a["kind"] != "program" {
		t.Fatalf("node's attributes = %v", a)
	}
	if kind, _ := points.At(2).Attributes().Get("kind"); kind.Str() != "unattributed" {
		t.Fatalf("a program not seen is %q", kind.Str())
	}
	if kind, _ := points.At(3).Attributes().Get("kind"); kind.Str() != "other" {
		t.Fatalf("the remaining programs are %q", kind.Str())
	}
	scan := metrics.At(1).Gauge().DataPoints().At(0)
	if privileged, _ := scan.Attributes().Get("privileged"); scan.DoubleValue() != 60 || !privileged.Bool() {
		t.Fatalf("scan = %v %v", scan.DoubleValue(), privileged.Bool())
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.SampleInterval = 0
	if cfg.Validate() == nil {
		t.Fatal("a zero sample interval is refused")
	}
}
