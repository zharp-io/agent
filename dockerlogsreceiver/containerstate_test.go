package dockerlogsreceiver

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
)

// stateOf is what docker_state reported for one container.
type stateOf struct {
	resource map[string]any
	state    map[string]any
	restarts int64
	start    time.Time
	monotone bool
}

func statesByName(t *testing.T, md pmetric.Metrics) map[string]stateOf {
	t.Helper()
	out := map[string]stateOf{}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		s := stateOf{resource: rm.Resource().Attributes().AsRaw()}
		ms := rm.ScopeMetrics().At(0).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			switch m.Name() {
			case "container.state":
				dp := m.Gauge().DataPoints().At(0)
				if dp.IntValue() != 1 {
					t.Errorf("container.state value %d, want 1", dp.IntValue())
				}
				s.state = dp.Attributes().AsRaw()
			case "container.restarts":
				dp := m.Sum().DataPoints().At(0)
				s.restarts = dp.IntValue()
				s.start = dp.StartTimestamp().AsTime()
				s.monotone = m.Sum().IsMonotonic() && m.Sum().AggregationTemporality() == pmetric.AggregationTemporalityCumulative
			default:
				t.Errorf("unexpected metric %s", m.Name())
			}
		}
		out[s.resource["container.name"].(string)] = s
	}
	return out
}

func TestStateMetrics(t *testing.T) {
	running := &containerInfo{ID: "aaa111", Name: "/api", Created: "2026-10-01T08:00:00.123456789Z", RestartCount: 3}
	running.Config.Image = "example/api:2.1"
	running.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}, "443/tcp": {}}
	running.State.Status = "running"
	running.State.StartedAt = "2026-10-30T09:15:00.5Z"
	running.State.FinishedAt = "0001-01-01T00:00:00Z"
	running.NetworkSettings.Ports = map[string][]portBinding{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "80"}}, "9090/udp": nil}

	dead := &containerInfo{ID: "bbb222", Name: "/worker", Created: "2026-09-01T00:00:00Z"}
	dead.Config.Image = "example/worker:1"
	dead.State.Status = "exited"
	dead.State.ExitCode = 137
	dead.State.OOMKilled = true
	dead.State.StartedAt = "2026-10-29T10:00:00Z"
	dead.State.FinishedAt = "2026-10-29T11:30:00Z"
	dead.HostConfig.PortBindings = map[string][]portBinding{"5432/tcp": {{HostPort: "5432"}}}

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	got := statesByName(t, stateMetrics([]*containerInfo{running, dead}, now))

	api := got["api"]
	wantRes := map[string]any{"container.id": "aaa111", "container.name": "api", "container.image.name": "example/api:2.1", "container.runtime": "docker"}
	for k, v := range wantRes {
		if api.resource[k] != v {
			t.Errorf("api resource %s = %v, want %v", k, api.resource[k], v)
		}
	}
	wantState := map[string]any{
		"container.state": "running", "container.exit_code": "0", "container.oom_killed": "false",
		"container.started_at": "2026-10-30T09:15:00Z", "container.finished_at": "",
		"container.created_at": "2026-10-01T08:00:00Z", "container.ports": "443/tcp,8080/tcp,9090/udp",
	}
	for k, v := range wantState {
		if api.state[k] != v {
			t.Errorf("api %s = %q, want %q", k, api.state[k], v)
		}
	}
	if api.restarts != 3 || !api.monotone || !api.start.Equal(time.Date(2026, 10, 1, 8, 0, 0, 123456789, time.UTC)) {
		t.Errorf("api restarts %d monotone %v start %v", api.restarts, api.monotone, api.start)
	}

	w := got["worker"]
	for k, v := range map[string]any{"container.state": "exited", "container.exit_code": "137", "container.oom_killed": "true",
		"container.finished_at": "2026-10-29T11:30:00Z", "container.ports": "5432/tcp"} {
		if w.state[k] != v {
			t.Errorf("worker %s = %q, want %q", k, w.state[k], v)
		}
	}
}

func TestStateReceiverListsStoppedContainers(t *testing.T) {
	f, endpoint := newFakeDocker(t)
	f.add("c1", "web", nil, false)
	f.add("c2", "batch", nil, false)
	f.mu.Lock()
	f.containers["c2"].info.State.Running = false
	f.containers["c2"].info.State.Status = "exited"
	f.containers["c2"].info.State.ExitCode = 1
	f.containers["c2"].info.RestartCount = 5
	f.containers["c1"].info.State.Status = "running"
	f.mu.Unlock()

	fac := NewStateFactory()
	cfg := fac.CreateDefaultConfig().(*StateConfig)
	cfg.Endpoint = endpoint
	cfg.CollectionInterval = time.Hour // the first scrape happens on Start
	sink := &consumertest.MetricsSink{}
	r, err := fac.CreateMetrics(context.Background(), receiver.Settings{ID: component.NewID(component.MustNewType(stateTypeStr)), TelemetrySettings: component.TelemetrySettings{Logger: zap.NewNop()}}, cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	waitFor(t, "a scrape", func() bool { return len(sink.AllMetrics()) > 0 })

	got := statesByName(t, sink.AllMetrics()[0])
	if len(got) != 2 {
		t.Fatalf("containers reported: %v", got)
	}
	if got["web"].state["container.state"] != "running" || got["web"].resource["container.image.name"] != "example/web:1.0" {
		t.Errorf("web: %+v", got["web"])
	}
	if got["batch"].state["container.state"] != "exited" || got["batch"].state["container.exit_code"] != "1" || got["batch"].restarts != 5 {
		t.Errorf("batch: %+v", got["batch"])
	}
}

func TestStateConfigValidate(t *testing.T) {
	cfg := NewStateFactory().CreateDefaultConfig().(*StateConfig)
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config: %v", err)
	}
	if cfg.CollectionInterval != 30*time.Second {
		t.Errorf("default interval %v", cfg.CollectionInterval)
	}
	bad := *cfg
	bad.Endpoint = "tcp://localhost:2375"
	if bad.Validate() == nil {
		t.Error("tcp endpoint accepted")
	}
}
