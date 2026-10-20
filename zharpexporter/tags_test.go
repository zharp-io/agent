package zharpexporter

import (
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

func TestResolveTags(t *testing.T) {
	cases := []struct {
		name     string
		resource map[string]string
		record   map[string]string
		want     serviceTags
	}{
		{"nothing set", nil, nil, serviceTags{}},
		{
			"resource attributes",
			map[string]string{AttrServiceName: "checkout", AttrEnvironment: "prod", AttrServiceVersion: "1.4.2", AttrSource: "nginx"},
			nil,
			serviceTags{"checkout", "prod", "1.4.2", "nginx"},
		},
		{
			"legacy environment key",
			map[string]string{AttrServiceName: "checkout", AttrEnvironmentOld: "staging"},
			nil,
			serviceTags{Service: "checkout", Env: "staging"},
		},
		{
			"current key wins over legacy",
			map[string]string{AttrEnvironmentOld: "old", AttrEnvironment: "new"},
			nil,
			serviceTags{Env: "new"},
		},
		{
			"record attributes fill the gaps",
			map[string]string{AttrServiceName: "api"},
			map[string]string{AttrServiceName: "ignored", AttrEnvironment: "prod", AttrSource: "python", AttrServiceVersion: "2"},
			serviceTags{"api", "prod", "2", "python"},
		},
		{
			"empty resource value falls through",
			map[string]string{AttrSource: ""},
			map[string]string{AttrSource: "redis"},
			serviceTags{Source: "redis"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, rec := pcommon.NewMap(), pcommon.NewMap()
			for k, v := range tc.resource {
				res.PutStr(k, v)
			}
			for k, v := range tc.record {
				rec.PutStr(k, v)
			}
			if got := resolveTags(res, rec); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestConvertLogsCarriesUnifiedTags(t *testing.T) {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr(AttrServiceName, "web")
	rl.Resource().Attributes().PutStr(AttrEnvironmentOld, "prod")
	rl.Resource().Attributes().PutStr(AttrSource, "nginx")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Body().SetStr(`127.0.0.1 - - [19/Oct/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 12 "-" "curl/8"`)
	lr.Attributes().PutStr(AttrServiceVersion, "3.1.0")

	// A record with no tags at all.
	bare := plog.NewLogs()
	bare.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hello")

	e := newLogsExporter(&Config{APIKey: "k", BatchSize: 10}, zap.NewNop())
	got := e.convertLogs(ld)
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	g := got[0]
	if g.Service != "web" || g.Env != "prod" || g.Version != "3.1.0" || g.Source != "nginx" {
		t.Errorf("tags: %+v", g)
	}

	b := e.convertLogs(bare)[0]
	if b.Service != "unknown" || b.Env != "" || b.Version != "" || b.Source != "" {
		t.Errorf("bare record: %+v", b)
	}
	// Untagged entries must serialise exactly as before: no env/version/source keys.
	raw, _ := json.Marshal(b)
	for _, k := range []string{`"env"`, `"version"`, `"source"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("untagged entry carries %s: %s", k, raw)
		}
	}
}

func TestConvertTracesCarriesEnvAndVersion(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(AttrServiceName, "checkout")
	rs.Resource().Attributes().PutStr(AttrEnvironment, "prod")
	rs.Resource().Attributes().PutStr(AttrServiceVersion, "9.9")
	rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("GET /")

	got := convertTraces(td)
	if got[0].Env != "prod" || got[0].ServiceVer != "9.9" || got[0].ServiceName != "checkout" {
		t.Errorf("span tags: %+v", got[0])
	}

	// Unset → omitted on the wire.
	bare := ptrace.NewTraces()
	bare.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("x")
	raw, _ := json.Marshal(convertTraces(bare)[0])
	if strings.Contains(string(raw), `"env"`) || strings.Contains(string(raw), `"serviceVersion"`) {
		t.Errorf("untagged span carries tags: %s", raw)
	}
}

func TestConvertMetricsNormalisesLegacyEnvironment(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr(AttrServiceName, "api")
	rm.Resource().Attributes().PutStr(AttrEnvironmentOld, "staging")
	rm.Resource().Attributes().PutStr(AttrServiceVersion, "1.0")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("x")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)

	attrs := convertMetrics(md)[0].Attributes
	if attrs[AttrEnvironment] != "staging" || attrs[AttrServiceVersion] != "1.0" || attrs[AttrServiceName] != "api" {
		t.Errorf("attributes: %v", attrs)
	}
}
