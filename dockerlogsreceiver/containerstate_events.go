package dockerlogsreceiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

// Why a container stopped is gone by the next scrape: Docker restarts a crashing container
// within moments and resets its exit code and OOM flag when it does, so a 30-second listing of
// a crash loop sees nothing but "running". docker_state therefore also follows Docker's event
// stream and reports each exit as it happens — a container.state point with the state
// "exited", the exit code from the die event, whether an oom event came just before it, and the
// start time of the run that ended — so Zharp can say "killed for running out of memory".

// oomWindow is how long before a die event an oom event still counts as its cause.
const oomWindow = 10 * time.Second

// stateEvent is a container event as the Engine API streams it.
type stateEvent struct {
	Type     string `json:"Type"`
	Action   string `json:"Action"`
	TimeNano int64  `json:"timeNano"`
	Actor    struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (e stateEvent) time() time.Time {
	if e.TimeNano > 0 {
		return time.Unix(0, e.TimeNano)
	}
	return time.Now()
}

// runTracker remembers, per container, when its current run started and when Docker last
// reported it out of memory.
type runTracker struct {
	mu     sync.Mutex
	starts map[string]time.Time
	ooms   map[string]time.Time
}

func newRunTracker() *runTracker {
	return &runTracker{starts: map[string]time.Time{}, ooms: map[string]time.Time{}}
}

// started records a run's start, from an event or a listing (whichever is newer).
func (t *runTracker) started(id string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at.After(t.starts[id]) {
		t.starts[id] = at
	}
}

func (t *runTracker) oom(id string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ooms[id] = at
}

// died returns the start of the run that ended and whether memory ran out just before.
func (t *runTracker) died(id string, at time.Time) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	oomAt, ok := t.ooms[id]
	delete(t.ooms, id)
	return t.starts[id], ok && at.Sub(oomAt) <= oomWindow && !oomAt.After(at.Add(time.Second))
}

func (t *runTracker) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.starts, id)
	delete(t.ooms, id)
}

// containerEvents streams start, die, oom and destroy events from now on.
func (c *dockerClient) containerEvents(ctx context.Context) (io.ReadCloser, error) {
	filters := `{"type":["container"],"event":["start","die","oom","destroy"]}`
	resp, err := c.stream(ctx, "/events?filters="+url.QueryEscape(filters))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// watchEvents follows the event stream until ctx ends, reconnecting with backoff.
func (r *stateReceiver) watchEvents(ctx context.Context) {
	backoff := time.Second
	for {
		began := time.Now()
		err := r.followEvents(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > time.Minute {
			backoff = time.Second
		}
		r.logger.Debug("docker_state: event stream ended; reconnecting", zap.Duration("retry_in", backoff), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (r *stateReceiver) followEvents(ctx context.Context) error {
	body, err := r.client.containerEvents(ctx)
	if err != nil {
		return err
	}
	defer body.Close()
	dec := json.NewDecoder(body)
	for {
		var e stateEvent
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("docker closed the event stream")
			}
			return err
		}
		if (e.Type != "" && e.Type != "container") || e.Actor.ID == "" {
			continue
		}
		switch e.Action {
		case "start":
			r.runs.started(e.Actor.ID, e.time())
		case "oom":
			r.runs.oom(e.Actor.ID, e.time())
		case "destroy":
			r.runs.forget(e.Actor.ID)
		case "die":
			start, oom := r.runs.died(e.Actor.ID, e.time())
			md := exitMetrics(e, start, oom)
			if err := r.next.ConsumeMetrics(ctx, md); err != nil {
				r.logger.Debug("docker_state: exit not delivered", zap.Error(err))
			}
		}
	}
}

// exitMetrics is one container.state point for a container that just stopped.
func exitMetrics(e stateEvent, start time.Time, oom bool) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	res := rm.Resource().Attributes()
	res.PutStr("container.id", e.Actor.ID)
	res.PutStr("container.name", strings.TrimPrefix(e.Actor.Attributes["name"], "/"))
	res.PutStr("container.image.name", e.Actor.Attributes["image"])
	res.PutStr("container.runtime", "docker")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(stateScopeName)
	m := sm.Metrics().AppendEmpty()
	m.SetName("container.state")
	m.SetDescription("The container's state; always 1, the details are attributes.")
	m.SetUnit("1")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	at := e.time()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(at))
	dp.SetIntValue(1)
	code := e.Actor.Attributes["exitCode"]
	if _, err := strconv.Atoi(code); err != nil {
		code = ""
	}
	a := dp.Attributes()
	a.PutStr("container.state", "exited")
	a.PutStr("container.exit_code", code)
	a.PutStr("container.oom_killed", strconv.FormatBool(oom))
	startedAt := ""
	if !start.IsZero() {
		startedAt = start.UTC().Format(time.RFC3339)
	}
	a.PutStr("container.started_at", startedAt)
	a.PutStr("container.finished_at", at.UTC().Format(time.RFC3339))
	a.PutStr("container.created_at", "")
	a.PutStr("container.ports", "")
	return md
}
