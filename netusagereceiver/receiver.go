// Package netusagereceiver is the zharp_network_usage receiver: each program's network traffic
// and who it talks to, for Servers & Containers' Network breakdown (Linux).
//
// Every sample_interval it reads the kernel's TCP connection table (package conns), and it hears
// each connection close with its final counters, so short requests count too. Which program holds
// a connection comes from the read-only helper /usr/local/lib/zharp-collector/zharp-inspect, which
// can read every process's open sockets (CAP_SYS_PTRACE); without it, only the collector's own
// user's programs are named and the rest are counted by user. Every collection_interval it reports:
//
//	zharp.network.io    bytes per second of one program with one peer, per direction
//	                    (process.executable.name, process.command_line, process.owner, kind,
//	                    network.role, network.peer.address, network.peer.name, network.peer.port,
//	                    network.scope, network.io.direction)
//	zharp.network.scan  the period measured, in seconds (privileged)
//
// Only these totals leave the host: never what was sent. Command lines are trimmed to the program
// and its first two arguments, as the processes pipeline does.
package netusagereceiver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/zharp-io/agent/netusagereceiver/conns"
)

const (
	typeStr   = "zharp_network_usage"
	scopeName = "github.com/zharp-io/agent/netusagereceiver"

	// DefaultHelper is where install puts the read-only helper.
	DefaultHelper = "/usr/local/lib/zharp-collector/zharp-inspect"
)

// Config is the zharp_network_usage receiver's configuration.
type Config struct {
	// CollectionInterval is how often the traffic is reported; SampleInterval, how often the
	// connection table is read.
	CollectionInterval time.Duration `mapstructure:"collection_interval"`
	SampleInterval     time.Duration `mapstructure:"sample_interval"`
	// Programs is how many programs are reported, the busiest; Peers, how many peers of each.
	Programs int `mapstructure:"programs"`
	Peers    int `mapstructure:"peers"`
	// ResolveNames looks up the names of public peers (reverse DNS, cached).
	ResolveNames bool `mapstructure:"resolve_names"`
	// Helper is the read-only helper; when it isn't there, the collector's own binary is asked.
	Helper string `mapstructure:"helper"`
}

func (c *Config) Validate() error {
	if c.SampleInterval < time.Second || c.CollectionInterval < c.SampleInterval {
		return errors.New("zharp_network_usage: sample_interval must be at least 1s and collection_interval at least sample_interval")
	}
	if c.Programs < 1 || c.Peers < 1 {
		return errors.New("zharp_network_usage: programs and peers must be positive")
	}
	return nil
}

// NewFactory creates the zharp_network_usage receiver factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		func() component.Config {
			return &Config{
				CollectionInterval: time.Minute,
				SampleInterval:     10 * time.Second,
				Programs:           12,
				Peers:              5,
				ResolveNames:       true,
				Helper:             DefaultHelper,
			}
		},
		receiver.WithMetrics(create, component.StabilityLevelAlpha),
	)
}

func create(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &netReceiver{cfg: c, logger: set.Logger, next: next, names: map[string]string{}}, nil
}

type netReceiver struct {
	cfg    *Config
	logger *zap.Logger
	next   consumer.Metrics
	cancel context.CancelFunc
	wg     sync.WaitGroup

	meter      *conns.Meter
	helper     *helper
	byInode    map[uint32]conns.Program
	tried      map[uint32]bool
	privileged bool
	failed     bool
	names      map[string]string
}

func (r *netReceiver) Start(_ context.Context, _ component.Host) error {
	if !conns.Supported() {
		r.logger.Info("zharp_network_usage: measuring connections is only supported on Linux")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.meter = conns.NewMeter()
	r.helper = &helper{bin: r.cfg.Helper}
	r.byInode = map[uint32]conns.Program{}
	r.tried = map[uint32]bool{}

	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		if err := conns.Watch(ctx, r.meter.Closed); err != nil && ctx.Err() == nil {
			// Old kernels: connections that open and close between samples go uncounted.
			r.logger.Info("zharp_network_usage: closed connections can't be watched", zap.Error(err))
		}
	}()
	go func() {
		defer r.wg.Done()
		defer r.helper.stop()
		sample := time.NewTicker(r.cfg.SampleInterval)
		defer sample.Stop()
		report := time.NewTicker(r.cfg.CollectionInterval)
		defer report.Stop()
		r.sample(ctx)
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sample.C:
				r.sample(ctx)
			case now := <-report.C:
				r.report(ctx, now.Sub(last), now)
				last = now
			}
		}
	}()
	return nil
}

func (r *netReceiver) Shutdown(context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}

func (r *netReceiver) sample(ctx context.Context) {
	sockets, err := conns.Dump()
	if err != nil {
		if !r.failed {
			r.logger.Warn("zharp_network_usage: the connection table can't be read", zap.Error(err))
		}
		r.failed = true
		return
	}
	r.failed = false
	// Ask the helper again only when a connection appeared whose process it hasn't been asked about.
	for _, s := range sockets {
		if _, known := r.byInode[s.Inode]; s.Inode != 0 && !known && !r.tried[s.Inode] {
			r.refreshOwners(ctx, sockets)
			break
		}
	}
	r.meter.Sample(sockets, r.programOf)
}

func (r *netReceiver) refreshOwners(ctx context.Context, sockets []conns.Socket) {
	owners, err := r.helper.owners(ctx)
	if err != nil {
		r.logger.Debug("zharp_network_usage: the helper didn't answer", zap.Error(err))
		owners = conns.Owners{}
	}
	r.privileged = owners.Privileged
	byInode := make(map[uint32]conns.Program, len(r.byInode))
	for _, p := range owners.Processes {
		program := conns.Program{Name: p.Name, CommandLine: p.CommandLine, Owner: p.Owner}
		for _, inode := range p.Inodes {
			byInode[inode] = program
		}
	}
	r.byInode = byInode
	r.tried = map[uint32]bool{}
	for _, s := range sockets {
		if _, ok := byInode[s.Inode]; !ok && s.Inode != 0 {
			r.tried[s.Inode] = true
		}
	}
}

// programOf names a connection's program, or at least its user.
func (r *netReceiver) programOf(s conns.Socket) conns.Program {
	if p, ok := r.byInode[s.Inode]; ok {
		return p
	}
	if s.Inode == 0 {
		return conns.Program{}
	}
	return conns.Program{Owner: conns.UserName(s.UID)}
}

func (r *netReceiver) report(ctx context.Context, period time.Duration, now time.Time) {
	flows := r.meter.Take(r.cfg.Programs, r.cfg.Peers)
	names := map[string]string{}
	if r.cfg.ResolveNames {
		for _, f := range flows {
			if f.Role == "client" && f.Scope == "public" && f.Peer != "" {
				names[f.Peer] = r.nameOf(ctx, f.Peer)
			}
		}
	}
	if err := r.next.ConsumeMetrics(ctx, Metrics(flows, names, period, r.privileged, now)); err != nil {
		r.logger.Debug("zharp_network_usage: metrics not accepted", zap.Error(err))
	}
}

// nameOf is a public address's name (reverse DNS), looked up once.
func (r *netReceiver) nameOf(ctx context.Context, addr string) string {
	if name, ok := r.names[addr]; ok {
		return name
	}
	if len(r.names) > 5000 {
		r.names = map[string]string{}
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	name := ""
	if found, err := net.DefaultResolver.LookupAddr(ctx, addr); err == nil && len(found) > 0 {
		name = strings.TrimSuffix(found[0], ".")
	}
	r.names[addr] = name
	return name
}

// Metrics turns a period's flows into the receiver's metrics: bytes per second over period.
func Metrics(flows []conns.Flow, names map[string]string, period time.Duration, privileged bool, now time.Time) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	ts := pcommon.NewTimestampFromTime(now)
	seconds := period.Seconds()
	if seconds <= 0 {
		seconds = 1
	}

	traffic := sm.Metrics().AppendEmpty()
	traffic.SetName("zharp.network.io")
	traffic.SetUnit("By/s")
	traffic.SetEmptyGauge()
	for _, f := range flows {
		kind := "program"
		if f.Other {
			kind = "other"
		} else if f.Program.Name == "" {
			kind = "unattributed"
		}
		for _, d := range []struct {
			direction string
			bytes     uint64
		}{{"transmit", f.Sent}, {"receive", f.Received}} {
			if d.bytes == 0 {
				continue
			}
			dp := traffic.Gauge().DataPoints().AppendEmpty()
			dp.SetTimestamp(ts)
			dp.SetDoubleValue(float64(d.bytes) / seconds)
			a := dp.Attributes()
			a.PutStr("process.executable.name", f.Program.Name)
			a.PutStr("process.command_line", f.Program.CommandLine)
			a.PutStr("process.owner", f.Program.Owner)
			a.PutStr("kind", kind)
			a.PutStr("network.role", f.Role)
			a.PutStr("network.peer.address", f.Peer)
			a.PutStr("network.peer.name", names[f.Peer])
			a.PutInt("network.peer.port", int64(f.Port))
			a.PutStr("network.scope", f.Scope)
			a.PutStr("network.io.direction", d.direction)
		}
	}

	scan := sm.Metrics().AppendEmpty()
	scan.SetName("zharp.network.scan")
	scan.SetUnit("s")
	dp := scan.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(ts)
	dp.SetDoubleValue(seconds)
	dp.Attributes().PutBool("privileged", privileged)
	return md
}

// helper is the read-only helper, running as one long-lived child (`inspect sockets --serve`):
// each line sent asks who holds which socket, and it answers with a line of JSON.
type helper struct {
	bin   string
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
}

func (h *helper) owners(ctx context.Context) (conns.Owners, error) {
	if h.cmd == nil {
		if err := h.start(); err != nil {
			return conns.Owners{}, err
		}
	}
	if _, err := io.WriteString(h.in, "\n"); err != nil {
		h.stop()
		return conns.Owners{}, err
	}
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.stop()
			return conns.Owners{}, errors.New("the helper stopped")
		}
		var owners conns.Owners
		err := json.Unmarshal(line, &owners)
		return owners, err
	case <-time.After(10 * time.Second):
		h.stop()
		return conns.Owners{}, errors.New("the helper took too long")
	case <-ctx.Done():
		return conns.Owners{}, ctx.Err()
	}
}

func (h *helper) start() error {
	bin := h.bin
	if _, err := os.Stat(bin); err != nil || bin == "" {
		if bin, err = os.Executable(); err != nil {
			return err
		}
	}
	cmd := exec.Command(bin, "inspect", "sockets", "--serve")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lines := make(chan []byte, 1)
	go func() {
		defer close(lines)
		reader := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	h.cmd, h.in, h.lines = cmd, in, lines
	return nil
}

func (h *helper) stop() {
	if h.cmd == nil {
		return
	}
	_ = h.in.Close()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
	h.cmd = nil
}
