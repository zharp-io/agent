package zharpexporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"math"
	"runtime"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

type metricPoint struct {
	Name       string         `json:"name"`
	Timestamp  string         `json:"timestamp"`
	Value      float64        `json:"value"`
	Type       string         `json:"type"`
	Unit       string         `json:"unit,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type metricsPayload struct {
	Hostname string        `json:"hostname"`
	OS       string        `json:"os"`
	Version  string        `json:"version"`
	Metrics  []metricPoint `json:"metrics"`
}

type zharpMetricsExporter struct {
	cfg      *Config
	logger   *zap.Logger
	hostname string
	cancel   context.CancelFunc
}

func newMetricsExporter(cfg *Config, logger *zap.Logger) *zharpMetricsExporter {
	h, _ := hostnameFunc()
	return &zharpMetricsExporter{cfg: cfg, logger: logger, hostname: h}
}

func (e *zharpMetricsExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (e *zharpMetricsExporter) Start(_ context.Context, _ component.Host) error {
	e.logger.Info("Zharp metrics exporter started", zap.String("endpoint", e.cfg.baseURL()))
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	if e.cfg.Database == "" {
		go e.heartbeatLoop(ctx)
	}
	return nil
}

func (e *zharpMetricsExporter) Shutdown(_ context.Context) error {
	if e.cancel != nil {
		e.cancel()
	}
	return nil
}

func (e *zharpMetricsExporter) heartbeatLoop(ctx context.Context) {
	e.sendHeartbeat(ctx)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.sendHeartbeat(ctx)
		}
	}
}

func (e *zharpMetricsExporter) sendHeartbeat(ctx context.Context) {
	payload := map[string]any{
		"hostname": e.hostname,
		"os":       runtime.GOOS,
		"version":  AgentVersion,
	}
	if services := readInventory(e.cfg.InventoryFile); services != nil {
		payload["services"] = services
	}
	if e.cfg.Kind != "" {
		payload["kind"] = e.cfg.Kind
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	url := fmt.Sprintf("%s/agent/heartbeat/%s", e.cfg.baseURL(), e.cfg.APIKey)
	req, err := newRequest(ctx, url, bytes.NewReader(body), e.cfg.APIKey)
	if err != nil {
		e.logger.Debug("heartbeat: create request failed", zap.Error(err))
		return
	}
	resp, err := defaultHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		e.logger.Debug("heartbeat: request failed", zap.Error(err))
		return
	}
	resp.Body.Close()
	e.logger.Debug("heartbeat sent", zap.String("hostname", e.hostname))
}

func (e *zharpMetricsExporter) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	points := convertMetrics(md)
	if len(points) == 0 {
		return nil
	}
	stampDatabase(points, e.cfg.Database)
	for i := 0; i < len(points); i += e.cfg.BatchSize {
		end := i + e.cfg.BatchSize
		if end > len(points) {
			end = len(points)
		}
		if err := e.shipMetrics(ctx, points[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (e *zharpMetricsExporter) shipMetrics(ctx context.Context, batch []metricPoint) error {
	p := metricsPayload{
		Hostname: e.hostname,
		OS:       runtime.GOOS,
		Version:  AgentVersion,
		Metrics:  batch,
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("zharpexporter: marshal metrics: %w", err)
	}

	url := fmt.Sprintf("%s/agent/metrics/%s", e.cfg.baseURL(), e.cfg.APIKey)
	req, err := newRequest(ctx, url, bytes.NewReader(body), e.cfg.APIKey)
	if err != nil {
		return err
	}

	resp, err := defaultHTTPClient(e.cfg.Timeout).Do(req)
	if err != nil {
		return fmt.Errorf("zharpexporter: metrics request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("zharpexporter: metrics server returned %d", resp.StatusCode)
	}

	e.logger.Debug("shipped metrics to Zharp", zap.Int("count", len(batch)))
	return nil
}

// stampDatabase marks every point with the database it came from, for an exporter instance that
// carries one named database. Points of the default, unnamed receivers are left as they were.
func stampDatabase(points []metricPoint, database string) {
	if database == "" {
		return
	}
	for i := range points {
		if points[i].Attributes == nil {
			points[i].Attributes = map[string]any{}
		}
		points[i].Attributes[DatabaseAttribute] = database
	}
}

func convertMetrics(md pmetric.Metrics) []metricPoint {
	var points []metricPoint

	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		resourceAttrs := attrsToMap(rm.Resource().Attributes())
		// Resource attributes ride on every point, which is how service.name,
		// deployment.environment.name and service.version reach the backend. An SDK on the
		// older conventions sends deployment.environment; carry it under the current key too.
		normaliseEnvAttr(resourceAttrs)

		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				m := sm.Metrics().At(k)
				points = append(points, extractPoints(m, resourceAttrs)...)
			}
		}
	}
	return points
}

func extractPoints(m pmetric.Metric, resourceAttrs map[string]any) []metricPoint {
	name := m.Name()
	unit := m.Unit()

	mergeAttrs := func(dp map[string]any) map[string]any {
		out := make(map[string]any, len(resourceAttrs)+len(dp))
		for k, v := range resourceAttrs {
			out[k] = v
		}
		for k, v := range dp {
			out[k] = v
		}
		return out
	}

	var pts []metricPoint

	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for i := 0; i < m.Gauge().DataPoints().Len(); i++ {
			dp := m.Gauge().DataPoints().At(i)
			pts = append(pts, metricPoint{
				Name:       name,
				Timestamp:  tsStr(dp.Timestamp()),
				Value:      dpValue(dp.ValueType(), dp.IntValue(), dp.DoubleValue()),
				Type:       "gauge",
				Unit:       unit,
				Attributes: mergeAttrs(attrsToMap(dp.Attributes())),
			})
		}
	case pmetric.MetricTypeSum:
		mtype := "counter"
		if !m.Sum().IsMonotonic() {
			mtype = "sum"
		}
		for i := 0; i < m.Sum().DataPoints().Len(); i++ {
			dp := m.Sum().DataPoints().At(i)
			pts = append(pts, metricPoint{
				Name:       name,
				Timestamp:  tsStr(dp.Timestamp()),
				Value:      dpValue(dp.ValueType(), dp.IntValue(), dp.DoubleValue()),
				Type:       mtype,
				Unit:       unit,
				Attributes: mergeAttrs(attrsToMap(dp.Attributes())),
			})
		}
	case pmetric.MetricTypeHistogram:
		for i := 0; i < m.Histogram().DataPoints().Len(); i++ {
			dp := m.Histogram().DataPoints().At(i)
			val := 0.0
			if dp.Count() > 0 {
				val = dp.Sum() / float64(dp.Count())
			}
			pts = append(pts, metricPoint{
				Name:       name + ".avg",
				Timestamp:  tsStr(dp.Timestamp()),
				Value:      val,
				Type:       "histogram",
				Unit:       unit,
				Attributes: mergeAttrs(attrsToMap(dp.Attributes())),
			})
		}
	case pmetric.MetricTypeSummary:
		// What StatsD timers, histograms and distributions become: sent like a histogram (the
		// average, as name.avg) plus each quantile the summary carries as its own gauge
		// (name.p50, name.p95, …), so latency percentiles aren't lost.
		for i := 0; i < m.Summary().DataPoints().Len(); i++ {
			dp := m.Summary().DataPoints().At(i)
			attrs := mergeAttrs(attrsToMap(dp.Attributes()))
			val := 0.0
			if dp.Count() > 0 {
				val = dp.Sum() / float64(dp.Count())
			}
			pts = append(pts, metricPoint{
				Name:       name + ".avg",
				Timestamp:  tsStr(dp.Timestamp()),
				Value:      val,
				Type:       "histogram",
				Unit:       unit,
				Attributes: attrs,
			})
			for q := 0; q < dp.QuantileValues().Len(); q++ {
				qv := dp.QuantileValues().At(q)
				pts = append(pts, metricPoint{
					Name:       fmt.Sprintf("%s.p%s", name, strconv.FormatFloat(qv.Quantile()*100, 'f', -1, 64)),
					Timestamp:  tsStr(dp.Timestamp()),
					Value:      qv.Value(),
					Type:       "gauge",
					Unit:       unit,
					Attributes: attrs,
				})
			}
		}
	}
	return pts
}

func dpValue(vt pmetric.NumberDataPointValueType, iv int64, dv float64) float64 {
	if vt == pmetric.NumberDataPointValueTypeInt {
		return float64(iv)
	}
	if math.IsNaN(dv) || math.IsInf(dv, 0) {
		return 0
	}
	return dv
}

func tsStr(ts pcommon.Timestamp) string {
	t := ts.AsTime()
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func attrsToMap(attrs pcommon.Map) map[string]any {
	if attrs.Len() == 0 {
		return nil
	}
	m := make(map[string]any, attrs.Len())
	attrs.Range(func(k string, v pcommon.Value) bool {
		m[k] = v.AsString()
		return true
	})
	return m
}
