package zharpexporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

// recorder is a stand-in Zharp API that keeps every metrics payload and counts heartbeats.
type recorder struct {
	mu         sync.Mutex
	payloads   []metricsPayload
	heartbeats int
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case strings.Contains(req.URL.Path, "/agent/metrics/"):
			var p metricsPayload
			_ = json.NewDecoder(req.Body).Decode(&p)
			r.payloads = append(r.payloads, p)
		case strings.Contains(req.URL.Path, "/agent/heartbeat/"):
			r.heartbeats++
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postgresMetrics() pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("postgresql.database.name", "orders")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("postgresql.backends")
	m.SetEmptySum().DataPoints().AppendEmpty().SetIntValue(4)
	return md
}

func TestNamedDatabaseExporterStampsEveryPoint(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)
	exp := newMetricsExporter(&Config{APIKey: "k", Endpoint: srv.URL, Timeout: time.Second, BatchSize: 500, Database: "postgresql/orders"}, zap.NewNop())

	if err := exp.ConsumeMetrics(context.Background(), postgresMetrics()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.payloads) != 1 || len(rec.payloads[0].Metrics) != 1 {
		t.Fatalf("payloads: %+v", rec.payloads)
	}
	attrs := rec.payloads[0].Metrics[0].Attributes
	if attrs[DatabaseAttribute] != "postgresql/orders" || attrs["postgresql.database.name"] != "orders" {
		t.Errorf("attributes: %v", attrs)
	}
}

func TestDefaultExporterLeavesPointsAsTheyWere(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)
	exp := newMetricsExporter(&Config{APIKey: "k", Endpoint: srv.URL, Timeout: time.Second, BatchSize: 500}, zap.NewNop())

	if err := exp.ConsumeMetrics(context.Background(), postgresMetrics()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.payloads[0].Metrics[0].Attributes[DatabaseAttribute]; ok {
		t.Error("an unnamed database was stamped")
	}
}

func TestNamedDatabaseExporterSendsNoHeartbeat(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)
	exp := newMetricsExporter(&Config{APIKey: "k", Endpoint: srv.URL, Timeout: time.Second, BatchSize: 500, Database: "redis/cache"}, zap.NewNop())
	if err := exp.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = exp.Shutdown(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.heartbeats != 0 {
		t.Errorf("named exporter sent %d heartbeats; the main exporter owns them", rec.heartbeats)
	}
}
