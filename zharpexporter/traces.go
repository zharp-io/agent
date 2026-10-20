package zharpexporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

// spanRecord is one span on the wire.
//
// This is the flat shape Zharp's ingest accepts, not OTLP/JSON. OTLP would mean re-encoding
// every attribute as a tagged union and every timestamp as a nanosecond string, so the backend
// could immediately decode both back again — bytes and work spent to arrive at the same place.
// The collector already holds decoded pdata, so it sends what it has.
type spanRecord struct {
	TraceID       string         `json:"traceId"`
	SpanID        string         `json:"spanId"`
	ParentSpanID  string         `json:"parentSpanId,omitempty"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	ServiceName   string         `json:"serviceName"`
	Env           string         `json:"env,omitempty"`
	ServiceVer    string         `json:"serviceVersion,omitempty"`
	StartTime     string         `json:"startTime"`
	EndTime       string         `json:"endTime"`
	StatusCode    string         `json:"statusCode"`
	StatusMessage string         `json:"statusMessage,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Resource      map[string]any `json:"resource,omitempty"`
	Events        []spanEvent    `json:"events,omitempty"`
}

// spanEvent is a timestamped moment inside a span. An `exception` event carries the stack
// trace, which is what an incident links to when it shows the failure that raised it.
type spanEvent struct {
	Name       string         `json:"name"`
	Time       string         `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type tracesPayload struct {
	Hostname string       `json:"hostname"`
	OS       string       `json:"os"`
	Version  string       `json:"version"`
	Spans    []spanRecord `json:"spans"`
}

type zharpTracesExporter struct {
	cfg      *Config
	logger   *zap.Logger
	client   *http.Client
	hostname string
}

func newTracesExporter(cfg *Config, logger *zap.Logger) *zharpTracesExporter {
	hostname, _ := hostnameFunc()
	return &zharpTracesExporter{
		cfg:      cfg,
		logger:   logger,
		client:   defaultHTTPClient(cfg.Timeout),
		hostname: hostname,
	}
}

func (e *zharpTracesExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (e *zharpTracesExporter) Start(_ context.Context, _ component.Host) error {
	e.logger.Info("Zharp traces exporter started", zap.String("endpoint", e.cfg.baseURL()))
	return nil
}

func (e *zharpTracesExporter) Shutdown(_ context.Context) error {
	return nil
}

// ConsumeTraces implements exporter.Traces.
func (e *zharpTracesExporter) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	records := convertTraces(td)
	if len(records) == 0 {
		return nil
	}
	// A busy service produces spans far faster than log lines, so a single flush can hold far
	// more than one request should carry. Split on the same batch size the other signals use.
	for i := 0; i < len(records); i += e.cfg.BatchSize {
		end := i + e.cfg.BatchSize
		if end > len(records) {
			end = len(records)
		}
		if err := e.shipTraces(ctx, records[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (e *zharpTracesExporter) shipTraces(ctx context.Context, batch []spanRecord) error {
	p := tracesPayload{
		Hostname: e.hostname,
		OS:       runtime.GOOS,
		Version:  AgentVersion,
		Spans:    batch,
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("zharpexporter: marshal traces: %w", err)
	}

	url := fmt.Sprintf("%s/agent/traces/%s", e.cfg.baseURL(), e.cfg.APIKey)
	req, err := newRequest(ctx, url, bytes.NewReader(body), e.cfg.APIKey)
	if err != nil {
		return err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("zharpexporter: traces request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("zharpexporter: traces server returned %d", resp.StatusCode)
	}

	e.logger.Debug("shipped spans to Zharp", zap.Int("count", len(batch)))
	return nil
}

func convertTraces(td ptrace.Traces) []spanRecord {
	var records []spanRecord

	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		resource := spanAttrs(rs.Resource().Attributes())

		// service.name is the axis everything in the product groups by, so it is lifted out
		// of the resource bag and sent as its own field.
		resTags := resolveTags(rs.Resource().Attributes(), pcommon.NewMap())

		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				tags := resTags
				if tags.Service == "" || tags.Env == "" || tags.Version == "" {
					tags = resolveTags(rs.Resource().Attributes(), span.Attributes())
				}
				serviceName := tags.Service
				if serviceName == "" {
					serviceName = "unknown"
				}
				record := convertSpan(span, serviceName, resource)
				record.Env = tags.Env
				record.ServiceVer = tags.Version
				records = append(records, record)
			}
		}
	}

	return records
}

func convertSpan(span ptrace.Span, serviceName string, resource map[string]any) spanRecord {
	record := spanRecord{
		TraceID:     span.TraceID().String(),
		SpanID:      span.SpanID().String(),
		Name:        span.Name(),
		Kind:        spanKind(span.Kind()),
		ServiceName: serviceName,
		StartTime:   span.StartTimestamp().AsTime().UTC().Format(time.RFC3339Nano),
		EndTime:     span.EndTimestamp().AsTime().UTC().Format(time.RFC3339Nano),
		StatusCode:  statusCode(span.Status().Code()),
		Attributes:  spanAttrs(span.Attributes()),
		Resource:    resource,
	}

	// A root span has no parent, and that absence is what identifies the start of a trace.
	// pdata renders an empty ID as sixteen zeros rather than an empty string, so sending it
	// unchecked would give every root a parent that does not exist.
	if !span.ParentSpanID().IsEmpty() {
		record.ParentSpanID = span.ParentSpanID().String()
	}

	if msg := span.Status().Message(); msg != "" {
		record.StatusMessage = msg
	}

	if n := span.Events().Len(); n > 0 {
		record.Events = make([]spanEvent, 0, n)
		for i := 0; i < n; i++ {
			ev := span.Events().At(i)
			record.Events = append(record.Events, spanEvent{
				Name:       ev.Name(),
				Time:       ev.Timestamp().AsTime().UTC().Format(time.RFC3339Nano),
				Attributes: spanAttrs(ev.Attributes()),
			})
		}
	}

	return record
}

// spanKind names which side of a call a span represents. pdata prints "Unspecified" for an
// unset kind, which is not one of the five OTel names — treat it as internal work, which is
// what an instrumentation that never set a kind is describing.
func spanKind(kind ptrace.SpanKind) string {
	switch kind {
	case ptrace.SpanKindServer:
		return "SERVER"
	case ptrace.SpanKindClient:
		return "CLIENT"
	case ptrace.SpanKindProducer:
		return "PRODUCER"
	case ptrace.SpanKindConsumer:
		return "CONSUMER"
	default:
		return "INTERNAL"
	}
}

// statusCode is UNSET unless the instrumentation said otherwise. UNSET is not success: only an
// explicit Ok is, and conflating the two would hide every failure nobody marked.
func statusCode(code ptrace.StatusCode) string {
	switch code {
	case ptrace.StatusCodeOk:
		return "OK"
	case ptrace.StatusCodeError:
		return "ERROR"
	default:
		return "UNSET"
	}
}

// spanAttrs flattens an attribute map while keeping each value's type.
//
// The logs and metrics paths stringify attributes, which is harmless for a label. Span
// attributes are not labels: http.status_code, db.rows_affected and the rest are filtered and
// compared as numbers on the way back out, and "200" does not compare like 200. So each value
// is emitted as what it is, and JSON carries the distinction the rest of the way.
func spanAttrs(attrs pcommon.Map) map[string]any {
	if attrs.Len() == 0 {
		return nil
	}
	out := make(map[string]any, attrs.Len())
	attrs.Range(func(k string, v pcommon.Value) bool {
		out[k] = attrValue(v)
		return true
	})
	return out
}

func attrValue(v pcommon.Value) any {
	switch v.Type() {
	case pcommon.ValueTypeInt:
		return v.Int()
	case pcommon.ValueTypeDouble:
		return v.Double()
	case pcommon.ValueTypeBool:
		return v.Bool()
	case pcommon.ValueTypeSlice:
		items := v.Slice()
		out := make([]any, 0, items.Len())
		for i := 0; i < items.Len(); i++ {
			out = append(out, attrValue(items.At(i)))
		}
		return out
	case pcommon.ValueTypeMap:
		return spanAttrs(v.Map())
	case pcommon.ValueTypeEmpty:
		return nil
	default:
		return v.Str()
	}
}
