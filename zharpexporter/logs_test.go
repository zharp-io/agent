package zharpexporter

import (
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// A log written inside a span keeps its trace and span, so the record links to its trace.
func TestConvertLogsCarriesTraceAndSpanID(t *testing.T) {
	ld := plog.NewLogs()
	sl := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
	lr := sl.LogRecords().AppendEmpty()
	lr.Body().SetStr("charged card")
	lr.SetTraceID(pcommon.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36})
	lr.SetSpanID(pcommon.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7})
	sl.LogRecords().AppendEmpty().Body().SetStr("no span")

	e := newLogsExporter(&Config{APIKey: "k", BatchSize: 10}, zap.NewNop())
	got := e.convertLogs(ld)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || got[0].SpanID != "00f067aa0ba902b7" {
		t.Errorf("ids: trace %q span %q", got[0].TraceID, got[0].SpanID)
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"`) || !strings.Contains(string(raw), `"spanId":"00f067aa0ba902b7"`) {
		t.Errorf("payload: %s", raw)
	}
	// A record outside any span serialises as before: no traceId/spanId keys.
	raw, _ = json.Marshal(got[1])
	if strings.Contains(string(raw), "traceId") || strings.Contains(string(raw), "spanId") {
		t.Errorf("record without a span carries ids: %s", raw)
	}
}
