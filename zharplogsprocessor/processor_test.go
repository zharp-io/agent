package zharplogsprocessor

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/zharp-io/agent/logrules"
)

func newLogs(t *testing.T, cfg *Config) (processor.Logs, *consumertest.LogsSink) {
	t.Helper()
	sink := new(consumertest.LogsSink)
	p, err := NewFactory().CreateLogs(context.Background(), processortest.NewNopSettings(NewFactory().Type()), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, sink
}

// podLogs is a batch of lines from one container, the way filelog + k8sattributes deliver it.
func podLogs(res map[string]string, stream string, lines ...string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	for k, v := range res {
		rl.Resource().Attributes().PutStr(k, v)
	}
	sl := rl.ScopeLogs().AppendEmpty()
	for _, l := range lines {
		lr := sl.LogRecords().AppendEmpty()
		lr.Body().SetStr(l)
		lr.Attributes().PutStr("log.iostream", stream)
		lr.Attributes().PutStr("log.file.path", "/var/log/pods/x/0.log")
	}
	return ld
}

type rec struct {
	res  map[string]any
	body string
	sev  string
}

func records(sink *consumertest.LogsSink) []rec {
	var out []rec
	for _, ld := range sink.AllLogs() {
		for i := 0; i < ld.ResourceLogs().Len(); i++ {
			rl := ld.ResourceLogs().At(i)
			for j := 0; j < rl.ScopeLogs().Len(); j++ {
				lrs := rl.ScopeLogs().At(j).LogRecords()
				for k := 0; k < lrs.Len(); k++ {
					out = append(out, rec{rl.Resource().Attributes().AsRaw(), lrs.At(k).Body().AsString(), lrs.At(k).SeverityText()})
				}
			}
		}
	}
	return out
}

func waitRecords(t *testing.T, sink *consumertest.LogsSink, n int) []rec {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.LogRecordCount() >= n {
			time.Sleep(100 * time.Millisecond)
			return records(sink)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out: %d records, want %d: %+v", sink.LogRecordCount(), n, records(sink))
	return nil
}

func TestGlobalRulesAndDefaultMasks(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.Rules = []logrules.Rule{
		{Type: "exclude", Pattern: `GET /healthz`},
		{Type: "mask", Pattern: `(password=)\S+`, Replace: "${1}***"},
	}
	cfg.Environment = "production"
	p, sink := newLogs(t, cfg)
	ld := podLogs(map[string]string{"service.name": "nginx"}, "stdout",
		`10.0.0.1 "GET /healthz" 200`,
		`login password=hunter2 card 4111 1111 1111 1111`,
		`Authorization: Bearer abcdefghijklmnop`,
	)
	if err := p.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}
	got := records(sink)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].body != "login password=*** card [REDACTED:card]" || got[1].body != "Authorization: Bearer [REDACTED]" {
		t.Fatalf("masking: %+v", got)
	}
	if got[0].res["deployment.environment.name"] != "production" || got[0].res["service.name"] != "nginx" {
		t.Fatalf("resource: %+v", got[0].res)
	}
}

func TestDefaultMasksOff(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.DefaultMasks = false
	p, sink := newLogs(t, cfg)
	_ = p.ConsumeLogs(context.Background(), podLogs(nil, "stdout", "key AKIAIOSFODNN7EXAMPLE"))
	if got := records(sink); got[0].body != "key AKIAIOSFODNN7EXAMPLE" {
		t.Fatalf("masked with default_masks: false: %+v", got)
	}
}

func k8sCfg() *Config {
	cfg := createDefaultConfig().(*Config)
	cfg.Kubernetes.Enabled = true
	cfg.Kubernetes.FlushTimeout = 300 * time.Millisecond
	cfg.Environment = "cluster-default"
	return cfg
}

func TestKubernetesContractAndJoining(t *testing.T) {
	p, sink := newLogs(t, k8sCfg())
	checkout := map[string]string{
		"k8s.pod.uid": "uid-1", "k8s.pod.name": "checkout-6f7c-abcde", "k8s.namespace.name": "shop",
		"k8s.container.name": "app", "k8s.deployment.name": "checkout", "container.image.name": "eclipse-temurin",
		"k8s.pod.annotations.zharp.io/service":     "checkout-api",
		"k8s.pod.annotations.zharp.io/env":         "staging",
		"k8s.pod.annotations.zharp.io/logs-mask":   `[{"pattern":"(card_holder=)\\w+","replace":"${1}***"}]`,
		"k8s.pod.labels.app.kubernetes.io/version": "3.1.0",
	}
	// The trace arrives split across two batches, as a file tail would deliver it.
	_ = p.ConsumeLogs(context.Background(), podLogs(checkout, "stderr",
		"2026-10-19 12:00:01.123 ERROR [http-1] c.e.Checkout - charge failed card_holder=alice",
		"java.lang.IllegalStateException: Order 42 has no lines",
		"\tat com.example.shop.OrderService.total(OrderService.java:57)"))
	_ = p.ConsumeLogs(context.Background(), podLogs(checkout, "stderr",
		"Caused by: java.sql.SQLException: timeout",
		"\t... 52 common frames omitted",
		"2026-10-19 12:00:02.000 INFO [http-1] c.e.Checkout - next"))

	got := waitRecords(t, sink, 2)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	want := "2026-10-19 12:00:01.123 ERROR [http-1] c.e.Checkout - charge failed card_holder=***\n" +
		"java.lang.IllegalStateException: Order 42 has no lines\n" +
		"\tat com.example.shop.OrderService.total(OrderService.java:57)\n" +
		"Caused by: java.sql.SQLException: timeout\n" +
		"\t... 52 common frames omitted"
	if got[0].body != want || got[0].sev != "ERROR" {
		t.Fatalf("joined record:\n%q %s", got[0].body, got[0].sev)
	}
	if got[1].body != "2026-10-19 12:00:02.000 INFO [http-1] c.e.Checkout - next" || got[1].sev != "INFO" {
		t.Fatalf("second record: %+v", got[1])
	}
	r := got[0].res
	if r["service.name"] != "checkout-api" || r["deployment.environment.name"] != "staging" ||
		r["service.version"] != "3.1.0" || r["zharp.source"] != "java" {
		t.Fatalf("contract: %+v", r)
	}
	for k := range r {
		if strings.HasPrefix(k, "k8s.pod.annotations.zharp.io/") {
			t.Fatalf("annotation %s exported", k)
		}
	}
}

func TestKubernetesOptOutAndFallbacks(t *testing.T) {
	p, sink := newLogs(t, k8sCfg())
	_ = p.ConsumeLogs(context.Background(), podLogs(map[string]string{
		"k8s.pod.uid": "uid-2", "k8s.container.name": "noisy",
		"k8s.pod.annotations.zharp.io/logs": "false",
	}, "stdout", "secret chatter"))
	_ = p.ConsumeLogs(context.Background(), podLogs(map[string]string{
		"k8s.pod.uid": "uid-3", "k8s.container.name": "web", "k8s.statefulset.name": "web-sts",
		"container.image.name":                        "docker.io/library/nginx",
		"k8s.pod.annotations.zharp.io/logs-exclude":   `kube-probe`,
		"k8s.pod.annotations.zharp.io/logs-multiline": "off",
	}, "stdout", `10.0.0.9 "GET / HTTP/1.1" 200 "kube-probe/1.31"`, `10.0.0.8 "GET /cart HTTP/1.1" 200`, "  indented but multiline is off"))
	got := waitRecords(t, sink, 2)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, g := range got {
		if g.res["service.name"] != "web-sts" || g.res["zharp.source"] != "nginx" || g.res["deployment.environment.name"] != "cluster-default" {
			t.Fatalf("fallbacks: %+v", g.res)
		}
		if strings.Contains(g.body, "secret chatter") || strings.Contains(g.body, "kube-probe") {
			t.Fatalf("not dropped: %+v", g)
		}
	}
}

func TestShutdownFlushesPending(t *testing.T) {
	cfg := k8sCfg()
	cfg.Kubernetes.FlushTimeout = time.Hour
	sink := new(consumertest.LogsSink)
	p, err := NewFactory().CreateLogs(context.Background(), processortest.NewNopSettings(NewFactory().Type()), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Start(context.Background(), componenttest.NewNopHost())
	_ = p.ConsumeLogs(context.Background(), podLogs(map[string]string{"k8s.pod.uid": "u", "k8s.container.name": "c"}, "stdout", "panic: boom", "", "goroutine 1 [running]:"))
	if sink.LogRecordCount() != 0 {
		t.Fatal("sent before the record was complete")
	}
	_ = p.Shutdown(context.Background())
	if got := records(sink); len(got) != 1 || got[0].body != "panic: boom\n\ngoroutine 1 [running]:" {
		t.Fatalf("got %+v", got)
	}
}

func TestMetricsGetTheContract(t *testing.T) {
	sink := new(consumertest.MetricsSink)
	p, err := NewFactory().CreateMetrics(context.Background(), processortest.NewNopSettings(NewFactory().Type()), k8sCfg(), sink)
	if err != nil {
		t.Fatal(err)
	}
	md := pmetric.NewMetrics()
	res := md.ResourceMetrics().AppendEmpty().Resource().Attributes()
	res.PutStr("k8s.pod.uid", "u")
	res.PutStr("k8s.pod.name", "api-123")
	res.PutStr("k8s.deployment.name", "api")
	res.PutStr("k8s.pod.annotations.zharp.io/version", "9.9")
	if err := p.ConsumeMetrics(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	got := sink.AllMetrics()[0].ResourceMetrics().At(0).Resource().Attributes().AsRaw()
	if got["service.name"] != "api" || got["service.version"] != "9.9" || got["deployment.environment.name"] != "cluster-default" {
		t.Fatalf("%+v", got)
	}
}

func TestValidate(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Rules = []logrules.Rule{{Type: "mask", Pattern: "("}}
	if cfg.Validate() == nil {
		t.Fatal("bad pattern accepted")
	}
	cfg = createDefaultConfig().(*Config)
	cfg.DisableDefaultMasks = []string{"cards"}
	if cfg.Validate() == nil {
		t.Fatal("unknown mask name accepted")
	}
}
