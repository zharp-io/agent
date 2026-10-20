package dockerlogsreceiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"

	"github.com/zharp-io/agent/logrules"
)

const (
	scopeName = "github.com/zharp-io/agent/dockerlogsreceiver"

	batchLines    = 200
	batchInterval = 500 * time.Millisecond
	saveInterval  = 5 * time.Second
	// The state file is rewritten at least this often even when no line arrived, so its
	// saved_at says how long ago the collector was last running.
	heartbeatSave = 30 * time.Second
	maxBackoff    = 30 * time.Second
)

type dockerLogsReceiver struct {
	cfg      *Config
	logger   *zap.Logger
	next     consumer.Logs
	client   *dockerClient
	state    *stateStore
	self     selfMatcher
	findSelf func() selfMatcher
	now      func() time.Time

	startedAt time.Time
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	mu    sync.Mutex
	tails map[string]bool
	noted map[string]bool // containers already logged as skipped
}

func newReceiver(cfg *Config, logger *zap.Logger, next consumer.Logs) (*dockerLogsReceiver, error) {
	client, err := newDockerClient(cfg.Endpoint, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	return &dockerLogsReceiver{
		cfg:      cfg,
		logger:   logger,
		next:     next,
		client:   client,
		now:      time.Now,
		findSelf: detectSelf,
		tails:    map[string]bool{},
		noted:    map[string]bool{},
	}, nil
}

func (r *dockerLogsReceiver) Start(_ context.Context, _ component.Host) error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.startedAt = r.now()
	r.state = loadState(r.cfg.StateFile)
	r.self = r.findSelf()

	r.wg.Add(2)
	go func() { defer r.wg.Done(); r.run(ctx) }()
	go func() { defer r.wg.Done(); r.saveLoop(ctx) }()
	return nil
}

func (r *dockerLogsReceiver) Shutdown(context.Context) error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	r.wg.Wait()
	if err := r.state.save(true); err != nil {
		r.logger.Warn("Could not save the docker log positions", zap.Error(err))
	}
	return nil
}

func (r *dockerLogsReceiver) saveLoop(ctx context.Context) {
	t := time.NewTicker(saveInterval)
	defer t.Stop()
	last := time.Now()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			force := time.Since(last) >= heartbeatSave
			if force {
				last = time.Now()
			}
			if err := r.state.save(force); err != nil && !warned {
				warned = true
				r.logger.Warn("Could not save the docker log positions; a restart may start again from now",
					zap.String("state_file", r.cfg.StateFile), zap.Error(err))
			}
		}
	}
}

// run keeps a session with the daemon going, reconnecting with backoff when Docker restarts or
// isn't there yet.
func (r *dockerLogsReceiver) run(ctx context.Context) {
	backoff := time.Second
	for {
		began := time.Now()
		err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > time.Minute {
			backoff = time.Second
		}
		r.logger.Warn("Docker is not reachable; retrying", zap.String("endpoint", r.cfg.Endpoint),
			zap.Duration("retry_in", backoff), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session lists the running containers, follows each, and then follows the event stream for
// containers that start later. The event stream is opened first so a container starting
// between the two calls isn't missed (following one twice is prevented by the tails map).
func (r *dockerLogsReceiver) session(ctx context.Context) error {
	ev, err := r.client.events(ctx)
	if err != nil {
		return err
	}
	defer ev.Body.Close()

	running, err := r.client.listRunning(ctx)
	if err != nil {
		return err
	}
	r.logger.Info("Following Docker container logs", zap.Int("containers", len(running)))
	for _, c := range running {
		r.follow(ctx, c.ID, true)
	}

	dec := json.NewDecoder(ev.Body)
	for {
		var e dockerEvent
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("docker closed the event stream")
			}
			return err
		}
		if e.Type != "" && e.Type != "container" {
			continue
		}
		switch e.Action {
		case "start":
			r.follow(ctx, e.Actor.ID, false)
		case "destroy":
			r.state.forget(e.Actor.ID)
		}
	}
}

// follow starts reading one container unless it is already being read.
func (r *dockerLogsReceiver) follow(ctx context.Context, id string, atConnect bool) {
	r.mu.Lock()
	if r.tails[id] {
		r.mu.Unlock()
		return
	}
	r.tails[id] = true
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.tails, id)
			r.mu.Unlock()
		}()
		if err := r.tail(ctx, id, atConnect); err != nil && ctx.Err() == nil {
			r.logger.Debug("Stopped reading container logs", zap.String("container.id", short(id)), zap.Error(err))
		}
	}()
}

// containerMeta is what every record from one container carries, and how its lines are
// processed. It is worked out from the container's labels and environment each time the
// container is followed, so a recreated container picks up new labels.
type containerMeta struct {
	id, name, image string
	tty             bool
	contract        logrules.Meta
	settings        logrules.Settings
	problems        []error // label values that could not be used
}

func (r *dockerLogsReceiver) describe(info *containerInfo) containerMeta {
	name := strings.TrimPrefix(info.Name, "/")
	m := containerMeta{id: info.ID, name: name, image: logrules.ImageName(info.Config.Image), tty: info.Config.Tty}
	m.contract = logrules.ResolveDocker(info.Config.Labels, info.Config.Env, name, info.Config.Image)
	if m.contract.Multiline == "" && r.cfg != nil {
		m.contract.Multiline = r.cfg.Multiline
	}
	m.settings, m.problems = m.contract.Compile()
	return m
}

// imageName drops the tag and digest: container.image.name is "nginx", not "nginx:1.27".
func imageName(ref string) string { return logrules.ImageName(ref) }

// excluded says why a container is not read, or "" when it is.
func (r *dockerLogsReceiver) excluded(info *containerInfo) string {
	if r.self.is(info.ID) {
		return "it is the collector's own container"
	}
	label := r.cfg.ExcludeLabel
	if label == "" {
		label = logrules.LabelLogs
	}
	if v, ok := info.Config.Labels[label]; ok && strings.EqualFold(strings.TrimSpace(v), "false") {
		return "it is labelled " + label + "=false"
	}
	if logrules.ResolveDocker(info.Config.Labels, info.Config.Env, info.Name, info.Config.Image).Disabled {
		return "it is labelled " + logrules.LabelLogs + "=false"
	}
	return ""
}

// startPoint decides where a container's log is read from.
//   - Known from a previous run: from the saved position, minus what was already sent.
//   - Started while the collector runs: from its start, so its first lines aren't lost.
//   - Already running the very first time the collector looks: from now; history isn't replayed.
//   - Already running but unknown after a restart (it started, or first logged, while the
//     collector was down): from when the collector last saved its state.
func (r *dockerLogsReceiver) startPoint(info *containerInfo, atConnect bool) (time.Time, *resumeFilter) {
	if p, ok := r.state.get(info.ID); ok && !p.TS.IsZero() {
		return p.TS, &resumeFilter{pos: p}
	}
	started, _ := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	if !atConnect {
		if started.IsZero() {
			return r.now().Add(-time.Second), nil
		}
		return started.Add(-time.Second), nil
	}
	if prev, ok := r.state.previousRun(); ok {
		return prev, nil
	}
	return r.startedAt, nil
}

func (r *dockerLogsReceiver) tail(ctx context.Context, id string, atConnect bool) error {
	info, err := r.client.inspect(ctx, id)
	if err != nil {
		return err
	}
	if why := r.excluded(info); why != "" {
		r.noteOnce(id, "Not reading container logs: "+why, strings.TrimPrefix(info.Name, "/"))
		return nil
	}
	meta := r.describe(info)
	for _, p := range meta.problems {
		r.logger.Warn("Ignoring a zharp.logs label that could not be used", zap.String("container", meta.name), zap.Error(p))
	}
	since, filter := r.startPoint(info, atConnect)

	resp, err := r.client.logs(ctx, id, since)
	if err != nil {
		r.noteOnce(id, "Cannot read this container's logs (its logging driver may not support reading)", meta.name)
		return err
	}
	defer resp.Body.Close()

	raw := meta.tty || strings.Contains(resp.Header.Get("Content-Type"), "raw-stream")
	lines := make(chan logLine, batchLines)
	readErr := make(chan error, 1)
	go func() {
		emit := func(l logLine) error {
			if filter.skip(l.ts) {
				return nil
			}
			select {
			case lines <- l:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if raw {
			readErr <- readRaw(resp.Body, emit)
		} else {
			readErr <- readMultiplexed(resp.Body, emit)
		}
		close(lines)
	}()

	// Lines are joined into records (stack traces) per stream, then batched.
	joiners := map[string]*streamJoiner{}
	batch := make([]record, 0, batchLines)
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		r.deliver(meta, batch)
		batch = batch[:0]
	}
	collect := func(recs []record) {
		batch = append(batch, recs...)
		if len(batch) >= batchLines {
			flush()
		}
	}
	flushJoiners := func(all bool) {
		now := r.now()
		for _, sj := range joiners {
			if all {
				collect(sj.flush())
			} else {
				collect(sj.flushIdle(now))
			}
		}
	}
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				flushJoiners(true)
				flush()
				return <-readErr
			}
			sj := joiners[l.stream]
			if sj == nil {
				sj = newStreamJoiner(meta.settings.Multiline)
				joiners[l.stream] = sj
			}
			collect(sj.add(l, r.now()))
		case <-ticker.C:
			flushJoiners(false)
			flush()
		case <-ctx.Done():
			flushJoiners(true)
			flush()
			return ctx.Err()
		}
	}
}

// record is one joined record and the timestamps of every line in it, so the saved position
// covers all of them.
type record struct {
	logLine
	lineTS []time.Time
}

// streamJoiner joins one stream's lines into records.
type streamJoiner struct {
	j  *logrules.Joiner[logLine]
	ts []time.Time // timestamps of the lines not yet returned in a record, oldest first
}

func newStreamJoiner(m logrules.Multiline) *streamJoiner {
	return &streamJoiner{j: logrules.NewJoiner[logLine](m)}
}

func (s *streamJoiner) add(l logLine, now time.Time) []record {
	s.ts = append(s.ts, l.ts)
	return s.take(s.j.Add(l, l.text, now))
}

func (s *streamJoiner) flushIdle(now time.Time) []record {
	return s.take(s.j.FlushIdle(now, logrules.DefaultIdle))
}

func (s *streamJoiner) flush() []record { return s.take(s.j.Flush()) }

func (s *streamJoiner) take(recs []logrules.Record[logLine]) []record {
	if len(recs) == 0 {
		return nil
	}
	out := make([]record, 0, len(recs))
	for _, rec := range recs {
		n := rec.Lines
		if n > len(s.ts) {
			n = len(s.ts)
		}
		l := rec.Meta
		l.text = rec.Text
		out = append(out, record{logLine: l, lineTS: append([]time.Time(nil), s.ts[:n]...)})
		s.ts = s.ts[n:]
	}
	return out
}

// deliver applies the container's exclude and mask rules, hands its records to the pipeline
// and moves its saved position on (past excluded records too).
func (r *dockerLogsReceiver) deliver(meta containerMeta, batch []record) {
	var ts []time.Time
	kept := make([]logLine, 0, len(batch))
	for _, rec := range batch {
		ts = append(ts, rec.lineTS...)
		text, keep := meta.settings.Rules.Apply(rec.text)
		if !keep {
			continue
		}
		l := rec.logLine
		l.text = text
		kept = append(kept, l)
	}
	if len(kept) > 0 {
		ld := toLogs(meta, kept, r.now())
		if err := r.next.ConsumeLogs(context.Background(), ld); err != nil {
			r.logger.Warn("Dropped container log records the pipeline refused", zap.String("container", meta.name),
				zap.Int("records", len(kept)), zap.Error(err))
		}
	}
	r.state.advance(meta.id, ts)
}

func toLogs(meta containerMeta, batch []logLine, now time.Time) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	res := rl.Resource().Attributes()
	c := meta.contract
	res.PutStr(logrules.AttrService, c.Service)
	for k, v := range map[string]string{logrules.AttrEnvironment: c.Env, logrules.AttrVersion: c.Version, logrules.AttrSource: c.Source} {
		if v != "" {
			res.PutStr(k, v)
		}
	}
	res.PutStr("container.id", meta.id)
	res.PutStr("container.name", meta.name)
	res.PutStr("container.image.name", meta.image)
	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName(scopeName)
	records := sl.LogRecords()
	records.EnsureCapacity(len(batch))
	observed := pcommon.NewTimestampFromTime(now)
	for _, l := range batch {
		lr := records.AppendEmpty()
		ts := l.ts
		if ts.IsZero() {
			ts = now
		}
		lr.SetTimestamp(pcommon.NewTimestampFromTime(ts))
		lr.SetObservedTimestamp(observed)
		lr.Body().SetStr(l.text)
		if sev, txt := parseLevel(l.text); sev != plog.SeverityNumberUnspecified {
			lr.SetSeverityNumber(sev)
			lr.SetSeverityText(txt)
		}
		// Record attributes too: the Zharp exporter forwards these as each line's metadata.
		a := lr.Attributes()
		a.PutStr("container.id", short(meta.id))
		a.PutStr("container.name", meta.name)
		a.PutStr("container.image.name", meta.image)
		a.PutStr("stream", l.stream)
	}
	return ld
}

func (r *dockerLogsReceiver) noteOnce(id, msg, name string) {
	r.mu.Lock()
	seen := r.noted[id]
	r.noted[id] = true
	r.mu.Unlock()
	if !seen {
		r.logger.Info(msg, zap.String("container", name), zap.String("container.id", short(id)))
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
