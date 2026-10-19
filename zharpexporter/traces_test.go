package zharpexporter

import (
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func TestConvertTraces(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout-api")
	rs.Resource().Attributes().PutStr("host.name", "web-01")

	ss := rs.ScopeSpans().AppendEmpty()

	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	// Root: no parent, server kind, ok.
	root := ss.Spans().AppendEmpty()
	root.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	root.SetSpanID(pcommon.SpanID{1, 1, 1, 1, 1, 1, 1, 1})
	root.SetName("GET /checkout")
	root.SetKind(ptrace.SpanKindServer)
	root.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	root.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(240 * time.Millisecond)))
	root.Status().SetCode(ptrace.StatusCodeOk)
	root.Attributes().PutStr("http.method", "GET")
	root.Attributes().PutInt("http.status_code", 200)

	// Child: parented, client kind, failed, carries an exception event.
	child := ss.Spans().AppendEmpty()
	child.SetTraceID(root.TraceID())
	child.SetSpanID(pcommon.SpanID{2, 2, 2, 2, 2, 2, 2, 2})
	child.SetParentSpanID(root.SpanID())
	child.SetName("SELECT orders")
	child.SetKind(ptrace.SpanKindClient)
	child.SetStartTimestamp(pcommon.NewTimestampFromTime(start.Add(10 * time.Millisecond)))
	child.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(200 * time.Millisecond)))
	child.Status().SetCode(ptrace.StatusCodeError)
	child.Status().SetMessage("connection reset by peer")
	ev := child.Events().AppendEmpty()
	ev.SetName("exception")
	ev.SetTimestamp(pcommon.NewTimestampFromTime(start.Add(199 * time.Millisecond)))
	ev.Attributes().PutStr("exception.type", "ECONNRESET")

	got := convertTraces(td)
	if len(got) != 2 {
		t.Fatalf("want 2 spans, got %d", len(got))
	}

	r, c := got[0], got[1]

	if r.TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace id not hex: %q", r.TraceID)
	}
	// The root must have no parent — this is what identifies where a trace begins.
	if r.ParentSpanID != "" {
		t.Errorf("root got a parent: %q", r.ParentSpanID)
	}
	if r.Kind != "SERVER" || r.StatusCode != "OK" {
		t.Errorf("root kind/status wrong: %s/%s", r.Kind, r.StatusCode)
	}
	// Numeric attributes must stay numeric: "200" does not compare like 200 on the way out.
	if r.Attributes["http.status_code"] != int64(200) {
		t.Errorf("status code attribute lost its type: %#v", r.Attributes["http.status_code"])
	}
	if r.Attributes["http.method"] != "GET" {
		t.Errorf("string attribute wrong: %#v", r.Attributes["http.method"])
	}
	if r.ServiceName != "checkout-api" {
		t.Errorf("service name wrong: %q", r.ServiceName)
	}
	if r.Resource["host.name"] != "web-01" {
		t.Errorf("resource not carried: %v", r.Resource)
	}
	if r.StartTime != "2026-08-26T10:00:00Z" {
		t.Errorf("start time wrong: %q", r.StartTime)
	}

	if c.ParentSpanID != r.SpanID {
		t.Errorf("child parent %q != root span %q", c.ParentSpanID, r.SpanID)
	}
	if c.StatusCode != "ERROR" || c.StatusMessage != "connection reset by peer" {
		t.Errorf("child status wrong: %s/%s", c.StatusCode, c.StatusMessage)
	}
	if len(c.Events) != 1 || c.Events[0].Attributes["exception.type"] != "ECONNRESET" {
		t.Errorf("exception event lost: %+v", c.Events)
	}

	// Omitempty must actually drop the parent key, not send an empty string the backend
	// would have to special-case.
	body, _ := json.Marshal(r)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	if _, present := decoded["parentSpanId"]; present {
		t.Errorf("root serialised a parentSpanId key: %s", body)
	}
}

func TestConvertTracesUnsetKindAndStatus(t *testing.T) {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(pcommon.TraceID{9})
	span.SetSpanID(pcommon.SpanID{9})
	span.SetName("work")

	got := convertTraces(td)
	if got[0].Kind != "INTERNAL" {
		t.Errorf("unset kind should be INTERNAL, got %q", got[0].Kind)
	}
	// UNSET is not success. Calling it OK would hide every failure nobody marked.
	if got[0].StatusCode != "UNSET" {
		t.Errorf("unset status should stay UNSET, got %q", got[0].StatusCode)
	}
	if got[0].ServiceName != "unknown" {
		t.Errorf("missing service.name should fall back to unknown, got %q", got[0].ServiceName)
	}
}
