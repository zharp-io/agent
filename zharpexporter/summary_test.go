package zharpexporter

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// StatsD timers arrive as summaries; they must reach Zharp as an average plus percentiles.
func TestSummaryBecomesAverageAndPercentiles(t *testing.T) {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("checkout.latency")
	m.SetUnit("ms")
	dp := m.SetEmptySummary().DataPoints().AppendEmpty()
	dp.SetCount(4)
	dp.SetSum(400)
	for _, q := range [][2]float64{{0.5, 90}, {0.95, 180}, {0.99, 250}} {
		qv := dp.QuantileValues().AppendEmpty()
		qv.SetQuantile(q[0])
		qv.SetValue(q[1])
	}

	got := map[string]float64{}
	for _, p := range convertMetrics(md) {
		got[p.Name] = p.Value
	}
	want := map[string]float64{"checkout.latency.avg": 100, "checkout.latency.p50": 90, "checkout.latency.p95": 180, "checkout.latency.p99": 250}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %v, want %v (all: %v)", name, got[name], v, got)
		}
	}
}
