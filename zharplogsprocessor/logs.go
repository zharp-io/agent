package zharplogsprocessor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"

	"github.com/zharp-io/agent/logrules"
)

// logsProcessor applies the rules to every record and, for Kubernetes containers, joins lines
// into records first. A joined record is held until the next line shows it is complete or
// until it has been idle for the flush timeout; a background loop sends the idle ones.
type logsProcessor struct {
	*resolver
	rules *logrules.Ruleset // global rules and the built-in masks
	next  consumer.Logs
	idle  time.Duration
	now   func() time.Time

	mu      sync.Mutex
	streams map[string]*stream

	stop chan struct{}
	done chan struct{}
}

// stream is one container stream being joined.
type stream struct {
	j     *logrules.Joiner[entry]
	rules *logrules.Ruleset // the pod's own exclude and mask rules
}

// entry is a record's first line, with its resource and scope, kept until the record is done.
type entry struct{ ld plog.Logs }

type finished struct {
	rec   logrules.Record[entry]
	rules *logrules.Ruleset
}

func newLogsProcessor(cfg *Config, logger *zap.Logger, next consumer.Logs) (*logsProcessor, error) {
	rs, err := cfg.ruleset()
	if err != nil {
		return nil, err
	}
	return &logsProcessor{
		resolver: newResolver(cfg, logger),
		rules:    rs,
		next:     next,
		idle:     durationOr(cfg.Kubernetes.FlushTimeout, logrules.DefaultIdle),
		now:      time.Now,
		streams:  map[string]*stream{},
	}, nil
}

func (p *logsProcessor) start(context.Context, component.Host) error {
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go p.flushLoop()
	return nil
}

func (p *logsProcessor) shutdown(ctx context.Context) error {
	if p.stop == nil {
		return nil
	}
	close(p.stop)
	<-p.done
	p.stop = nil
	p.send(ctx, p.flush(true))
	return nil
}

func (p *logsProcessor) flushLoop() {
	defer close(p.done)
	tick := p.idle / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.send(context.Background(), p.flush(false))
		}
	}
}

// flush collects the records that have been idle long enough (every pending one with all),
// and forgets streams with nothing pending.
func (p *logsProcessor) flush(all bool) []finished {
	now := p.now()
	var out []finished
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, st := range p.streams {
		var recs []logrules.Record[entry]
		if all {
			recs = st.j.Flush()
		} else {
			recs = st.j.FlushIdle(now, p.idle)
		}
		for _, r := range recs {
			out = append(out, finished{r, st.rules})
		}
		if !st.j.Pending() {
			delete(p.streams, key)
		}
	}
	return out
}

func (p *logsProcessor) send(ctx context.Context, recs []finished) {
	if len(recs) == 0 {
		return
	}
	ld := p.emit(recs)
	if ld.LogRecordCount() == 0 {
		return
	}
	if err := p.next.ConsumeLogs(ctx, ld); err != nil {
		p.logger.Warn("Dropped joined log records the pipeline refused", zap.Int("records", ld.LogRecordCount()), zap.Error(err))
	}
}

func (p *logsProcessor) processLogs(_ context.Context, ld plog.Logs) (plog.Logs, error) {
	now := p.now()
	var done []finished
	ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
		rv := p.apply(rl.Resource())
		if rv.disabled {
			return true
		}
		if !rv.k8s || rv.settings.Multiline.Off() {
			rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
				sl.LogRecords().RemoveIf(func(lr plog.LogRecord) bool { return !p.process(lr, rv.settings.Rules, rv.k8s) })
				return sl.LogRecords().Len() == 0
			})
			return rl.ScopeLogs().Len() == 0
		}
		p.mu.Lock()
		for i := 0; i < rl.ScopeLogs().Len(); i++ {
			sl := rl.ScopeLogs().At(i)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				lr := sl.LogRecords().At(k)
				key := rv.key + "/" + attr(lr.Attributes(), "log.iostream")
				st := p.streams[key]
				if st == nil {
					st = &stream{j: logrules.NewJoiner[entry](rv.settings.Multiline)}
					st.j.SetLimits(p.cfg.Kubernetes.MaxLines, p.cfg.Kubernetes.MaxBytes)
					p.streams[key] = st
				}
				st.j.SetMode(rv.settings.Multiline)
				st.rules = rv.settings.Rules
				for _, r := range st.j.Add(newEntry(rl.Resource(), sl.Scope(), lr), lr.Body().AsString(), now) {
					done = append(done, finished{r, st.rules})
				}
			}
		}
		p.mu.Unlock()
		return true
	})
	if len(done) > 0 {
		p.emit(done).ResourceLogs().MoveAndAppendTo(ld.ResourceLogs())
	}
	if ld.ResourceLogs().Len() == 0 {
		return ld, processorhelper.ErrSkipProcessingData
	}
	return ld, nil
}

// process applies a source's rules, then the global ones and the built-in masks, to one
// record. It returns false when the record is excluded.
func (p *logsProcessor) process(lr plog.LogRecord, source *logrules.Ruleset, detectLevel bool) bool {
	if lr.Body().Type() != pcommon.ValueTypeStr {
		return true
	}
	text, keep := source.Apply(lr.Body().Str())
	if !keep {
		return false
	}
	if text, keep = p.rules.Apply(text); !keep {
		return false
	}
	lr.Body().SetStr(text)
	if detectLevel && lr.SeverityNumber() == plog.SeverityNumberUnspecified {
		if sev, txt := logrules.ParseLevel(text); sev != logrules.SeverityUnspecified {
			lr.SetSeverityNumber(plog.SeverityNumber(sev))
			lr.SetSeverityText(txt)
		}
	}
	return true
}

// emit turns finished records into logs: the first line's record with the joined text.
func (p *logsProcessor) emit(recs []finished) plog.Logs {
	out := plog.NewLogs()
	for _, f := range recs {
		ld := f.rec.Meta.ld
		lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
		lr.Body().SetStr(f.rec.Text)
		if !p.process(lr, f.rules, true) {
			continue
		}
		ld.ResourceLogs().MoveAndAppendTo(out.ResourceLogs())
	}
	return out
}

func newEntry(res pcommon.Resource, scope pcommon.InstrumentationScope, lr plog.LogRecord) entry {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	res.CopyTo(rl.Resource())
	sl := rl.ScopeLogs().AppendEmpty()
	scope.CopyTo(sl.Scope())
	lr.CopyTo(sl.LogRecords().AppendEmpty())
	return entry{ld: ld}
}

func attr(m pcommon.Map, k string) string {
	if v, ok := m.Get(k); ok {
		return v.AsString()
	}
	return ""
}
