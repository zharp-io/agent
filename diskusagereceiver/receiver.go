// Package diskusagereceiver is the zharp_disk_usage receiver: what takes each drive's space, for
// Servers & Containers. Hourly, it runs a scan (package scan) in a process of its own, through the
// read-only helper /usr/local/lib/zharp-collector/zharp-inspect when it's installed (it can list
// every folder) or the collector's own binary otherwise (it sees what its user can read), and
// reports:
//
//	zharp.disk.folder.size  bytes under a folder: each drive's biggest folders and their biggest
//	                        children (attributes: mount, path, parent, owner)
//	zharp.disk.owner.size   bytes each user's files take on a drive (mount, owner)
//	zharp.disk.scan         seconds the scan took (mount, complete, privileged, files, unreadable)
//
// Only these totals leave the host: never a file name below the reported folders, never content.
package diskusagereceiver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/zharp-io/agent/diskusagereceiver/scan"
)

const (
	typeStr   = "zharp_disk_usage"
	scopeName = "github.com/zharp-io/agent/diskusagereceiver"

	// DefaultHelper is where install puts the read-only helper.
	DefaultHelper = "/usr/local/lib/zharp-collector/zharp-inspect"
)

// Config is the zharp_disk_usage receiver's configuration.
type Config struct {
	// CollectionInterval is how often the drives are scanned.
	CollectionInterval time.Duration `mapstructure:"collection_interval"`
	// InitialDelay is how long after start the first scan runs.
	InitialDelay time.Duration `mapstructure:"initial_delay"`
	// Timeout bounds a scan; what was measured by then is reported, marked incomplete.
	Timeout time.Duration `mapstructure:"timeout"`
	// Top is how many top-level folders per drive are reported; Children, how many under each.
	Top      int `mapstructure:"top"`
	Children int `mapstructure:"children"`
	// Helper is the read-only helper; when it isn't there, the collector's own binary scans.
	Helper string `mapstructure:"helper"`
}

func (c *Config) Validate() error {
	if c.CollectionInterval < time.Minute {
		return errors.New("zharp_disk_usage: collection_interval must be at least 1m")
	}
	if c.Timeout <= 0 || c.Top < 1 || c.Children < 0 {
		return errors.New("zharp_disk_usage: timeout and top must be positive")
	}
	return nil
}

// NewFactory creates the zharp_disk_usage receiver factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		func() component.Config {
			return &Config{
				CollectionInterval: time.Hour,
				InitialDelay:       2 * time.Minute,
				Timeout:            4 * time.Minute,
				Top:                12,
				Children:           4,
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
	return &diskReceiver{cfg: c, logger: set.Logger, next: next}, nil
}

type diskReceiver struct {
	cfg    *Config
	logger *zap.Logger
	next   consumer.Metrics
	cancel context.CancelFunc
	wg     sync.WaitGroup
	failed bool
}

func (r *diskReceiver) Start(_ context.Context, _ component.Host) error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.InitialDelay):
		}
		t := time.NewTicker(r.cfg.CollectionInterval)
		defer t.Stop()
		for {
			r.scanOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (r *diskReceiver) Shutdown(context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}

func (r *diskReceiver) scanOnce(ctx context.Context) {
	res, err := r.run(ctx)
	if err != nil {
		if ctx.Err() == nil && !r.failed {
			r.logger.Warn("zharp_disk_usage: the scan failed", zap.Error(err))
		}
		r.failed = true
		return
	}
	r.failed = false
	if err := r.next.ConsumeMetrics(ctx, Metrics(res, time.Now())); err != nil {
		r.logger.Debug("zharp_disk_usage: metrics not accepted", zap.Error(err))
	}
}

// run scans in a process of its own: the helper's read-only access stays in the helper, and a
// scan's memory is returned the moment it ends.
func (r *diskReceiver) run(ctx context.Context) (scan.Result, error) {
	bin := r.cfg.Helper
	if _, err := os.Stat(bin); err != nil || bin == "" {
		if bin, err = os.Executable(); err != nil {
			return scan.Result{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout+time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "inspect", "disk", "--json",
		"--top", strconv.Itoa(r.cfg.Top), "--children", strconv.Itoa(r.cfg.Children),
		"--timeout", r.cfg.Timeout.String()).Output()
	if err != nil {
		return scan.Result{}, err
	}
	var res scan.Result
	if err := json.Unmarshal(out, &res); err != nil {
		return scan.Result{}, err
	}
	return res, nil
}

// Metrics turns a scan into the receiver's metrics.
func Metrics(res scan.Result, now time.Time) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	ts := pcommon.NewTimestampFromTime(now)

	folders := sm.Metrics().AppendEmpty()
	folders.SetName("zharp.disk.folder.size")
	folders.SetUnit("By")
	owners := sm.Metrics().AppendEmpty()
	owners.SetName("zharp.disk.owner.size")
	owners.SetUnit("By")
	scans := sm.Metrics().AppendEmpty()
	scans.SetName("zharp.disk.scan")
	scans.SetUnit("s")

	folder := func(mount, parent string, f scan.Folder) {
		dp := folders.Gauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetIntValue(f.Bytes)
		a := dp.Attributes()
		a.PutStr("mount", mount)
		a.PutStr("path", f.Path)
		a.PutStr("parent", parent)
		a.PutStr("owner", f.Owner)
	}
	folders.SetEmptyGauge()
	owners.SetEmptyGauge()
	scans.SetEmptyGauge()
	for _, m := range res.Mounts {
		for _, f := range m.Folders {
			folder(m.Mount, "", f)
			for _, c := range f.Children {
				folder(m.Mount, f.Path, c)
			}
		}
		for _, o := range m.Owners {
			dp := owners.Gauge().DataPoints().AppendEmpty()
			dp.SetTimestamp(ts)
			dp.SetIntValue(o.Bytes)
			dp.Attributes().PutStr("mount", m.Mount)
			dp.Attributes().PutStr("owner", o.Owner)
		}
		dp := scans.Gauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(ts)
		dp.SetDoubleValue(m.Seconds)
		a := dp.Attributes()
		a.PutStr("mount", m.Mount)
		a.PutBool("complete", m.Complete)
		a.PutBool("privileged", res.Privileged)
		a.PutInt("files", m.Files)
		a.PutInt("unreadable", m.Unreadable)
		a.PutInt("bytes", m.Bytes)
	}
	return md
}
