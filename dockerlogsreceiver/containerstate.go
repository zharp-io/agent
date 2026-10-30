package dockerlogsreceiver

// docker_state: each container's lifecycle, stopped ones included.
//
// docker_stats only scrapes running containers and reports usage, so a container that exited,
// was OOM-killed or keeps restarting simply disappears from it. This receiver lists every
// container through the same Engine API client as docker_logs and reports its state, exit code,
// OOM kill, start/finish/create times, ports and restart count. The resource attributes are the
// ones docker_stats uses (container.id, container.name, container.image.name, container.runtime),
// so Zharp joins the two on the same container.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
)

const (
	stateTypeStr   = "docker_state"
	stateScopeName = "github.com/zharp-io/agent/dockerlogsreceiver/dockerstatereceiver"
)

// StateConfig is the docker_state receiver's configuration.
type StateConfig struct {
	// Endpoint is the Docker Engine API socket, as for docker_logs.
	Endpoint string `mapstructure:"endpoint"`
	// CollectionInterval is how often every container is listed and inspected.
	CollectionInterval time.Duration `mapstructure:"collection_interval"`
	// Timeout bounds each API call.
	Timeout time.Duration `mapstructure:"timeout"`
}

func (c *StateConfig) Validate() error {
	e := strings.TrimSpace(c.Endpoint)
	if !strings.HasPrefix(e, "unix://") && !strings.HasPrefix(e, "npipe://") {
		return errors.New("docker_state: endpoint must be unix:///path/to/docker.sock or npipe:////./pipe/docker_engine")
	}
	if c.CollectionInterval <= 0 {
		return errors.New("docker_state: collection_interval must be positive")
	}
	if c.Timeout < 0 {
		return errors.New("docker_state: timeout must not be negative")
	}
	return nil
}

// NewStateFactory creates the docker_state receiver factory. The builder reaches it through the
// dockerstatereceiver package, since a module path names one factory.
func NewStateFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(stateTypeStr),
		func() component.Config {
			return &StateConfig{Endpoint: defaultEndpoint(), CollectionInterval: 30 * time.Second, Timeout: 10 * time.Second}
		},
		receiver.WithMetrics(createStateReceiver, component.StabilityLevelAlpha),
	)
}

func createStateReceiver(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*StateConfig)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	client, err := newDockerClient(c.Endpoint, c.Timeout)
	if err != nil {
		return nil, err
	}
	return &stateReceiver{cfg: c, logger: set.Logger, next: next, client: client, now: time.Now, runs: newRunTracker()}, nil
}

type stateReceiver struct {
	cfg    *StateConfig
	logger *zap.Logger
	next   consumer.Metrics
	client *dockerClient
	now    func() time.Time
	runs   *runTracker // run starts and OOM events, for the exits the event stream reports

	cancel context.CancelFunc
	wg     sync.WaitGroup
	failed bool // the last scrape failed; logged once until it works again
}

func (r *stateReceiver) Start(_ context.Context, _ component.Host) error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		r.watchEvents(ctx)
	}()
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.cfg.CollectionInterval)
		defer t.Stop()
		for {
			r.scrapeOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (r *stateReceiver) Shutdown(context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}

func (r *stateReceiver) scrapeOnce(ctx context.Context) {
	md, err := r.scrape(ctx)
	if err != nil {
		if ctx.Err() == nil && !r.failed {
			r.logger.Warn("docker_state: could not list containers", zap.Error(err))
		}
		r.failed = true
		return
	}
	r.failed = false
	if md.ResourceMetrics().Len() == 0 {
		return
	}
	if err := r.next.ConsumeMetrics(ctx, md); err != nil {
		r.logger.Debug("docker_state: metrics not accepted", zap.Error(err))
	}
}

func (r *stateReceiver) scrape(ctx context.Context) (pmetric.Metrics, error) {
	list, err := r.client.listAll(ctx)
	if err != nil {
		return pmetric.Metrics{}, err
	}
	infos := make([]*containerInfo, 0, len(list))
	for _, c := range list {
		info, err := r.client.inspect(ctx, c.ID)
		if err != nil {
			// Removed between the list and the inspect, most likely; it's gone from the next one.
			r.logger.Debug("docker_state: inspect failed", zap.String("container", c.ID), zap.Error(err))
			continue
		}
		infos = append(infos, info)
		if started, ok := parseDockerTime(info.State.StartedAt); ok {
			r.runs.started(info.ID, started)
		}
	}
	return stateMetrics(infos, r.now()), nil
}

// stateMetrics is one ResourceMetrics per container: container.state (1, with the details as
// attributes) and container.restarts.
func stateMetrics(infos []*containerInfo, now time.Time) pmetric.Metrics {
	md := pmetric.NewMetrics()
	ts := pcommon.NewTimestampFromTime(now)
	for _, c := range infos {
		rm := md.ResourceMetrics().AppendEmpty()
		res := rm.Resource().Attributes()
		res.PutStr("container.id", c.ID)
		res.PutStr("container.name", strings.TrimPrefix(c.Name, "/"))
		res.PutStr("container.image.name", c.Config.Image)
		res.PutStr("container.runtime", "docker")

		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName(stateScopeName)

		state := sm.Metrics().AppendEmpty()
		state.SetName("container.state")
		state.SetDescription("The container's state; always 1, the details are attributes.")
		state.SetUnit("1")
		dp := state.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetIntValue(1)
		a := dp.Attributes()
		a.PutStr("container.state", c.State.Status)
		a.PutStr("container.exit_code", strconv.Itoa(c.State.ExitCode))
		a.PutStr("container.oom_killed", strconv.FormatBool(c.State.OOMKilled))
		a.PutStr("container.started_at", dockerTime(c.State.StartedAt))
		a.PutStr("container.finished_at", dockerTime(c.State.FinishedAt))
		a.PutStr("container.created_at", dockerTime(c.Created))
		a.PutStr("container.ports", containerPorts(c))

		restarts := sm.Metrics().AppendEmpty()
		restarts.SetName("container.restarts")
		restarts.SetDescription("How many times Docker has restarted the container.")
		restarts.SetUnit("{restart}")
		sum := restarts.SetEmptySum()
		sum.SetIsMonotonic(true)
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		rp := sum.DataPoints().AppendEmpty()
		if created, ok := parseDockerTime(c.Created); ok {
			rp.SetStartTimestamp(pcommon.NewTimestampFromTime(created))
		}
		rp.SetTimestamp(ts)
		rp.SetIntValue(c.RestartCount)
	}
	return md
}

// parseDockerTime reads one of Docker's timestamps. Docker writes 0001-01-01T00:00:00Z for a
// time that hasn't happened (a container never started, or still running).
func parseDockerTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}, false
	}
	return t, true
}

// dockerTime is s as RFC 3339 in UTC, or "" when it is Docker's zero time or unreadable.
func dockerTime(s string) string {
	t, ok := parseDockerTime(s)
	if !ok {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// containerPorts is the container-side ports ("5432/tcp,8080/tcp"): published ones, bound ones
// (known even while the container is stopped) and the image's exposed ones, sorted and deduped.
func containerPorts(c *containerInfo) string {
	seen := map[string]bool{}
	for p := range c.NetworkSettings.Ports {
		seen[p] = true
	}
	for p := range c.HostConfig.PortBindings {
		seen[p] = true
	}
	for p := range c.Config.ExposedPorts {
		seen[p] = true
	}
	ports := make([]string, 0, len(seen))
	for p := range seen {
		if p != "" {
			ports = append(ports, p)
		}
	}
	// Numerically, so 8080/tcp comes after 443/tcp.
	sort.Slice(ports, func(i, j int) bool {
		ni, pi, _ := strings.Cut(ports[i], "/")
		nj, pj, _ := strings.Cut(ports[j], "/")
		a, errA := strconv.Atoi(ni)
		b, errB := strconv.Atoi(nj)
		if errA == nil && errB == nil && a != b {
			return a < b
		}
		if ni != nj {
			return ni < nj
		}
		return pi < pj
	})
	return strings.Join(ports, ",")
}
