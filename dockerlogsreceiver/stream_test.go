package dockerlogsreceiver

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func collect(t *testing.T, read func(func(logLine) error) error) []logLine {
	t.Helper()
	var out []logLine
	if err := read(func(l logLine) error { out = append(out, l); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDemultiplexesStdoutAndStderr(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, "2026-10-19T10:00:00.000000001Z hello from stdout\n"))
	b.Write(frame(2, "2026-10-19T10:00:00.000000002Z oops on stderr\n"))
	b.Write(frame(0, "ignored stdin\n"))
	b.Write(frame(1, "2026-10-19T10:00:00.000000003Z second\n"))

	lines := collect(t, func(emit func(logLine) error) error { return readMultiplexed(&b, emit) })
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	want := []struct{ stream, text string }{{"stdout", "hello from stdout"}, {"stderr", "oops on stderr"}, {"stdout", "second"}}
	for i, w := range want {
		if lines[i].stream != w.stream || lines[i].text != w.text {
			t.Errorf("line %d: got %s %q, want %s %q", i, lines[i].stream, lines[i].text, w.stream, w.text)
		}
	}
	if lines[1].ts.Nanosecond() != 2 {
		t.Errorf("timestamp not parsed: %v", lines[1].ts)
	}
}

func TestJoinsPartialMessages(t *testing.T) {
	// Docker splits messages over 16 KiB; each part carries its own timestamp.
	var b bytes.Buffer
	b.Write(frame(1, "2026-10-19T10:00:00.1Z first half "))
	b.Write(frame(2, "2026-10-19T10:00:00.15Z interleaved stderr\n"))
	b.Write(frame(1, "2026-10-19T10:00:00.2Z second half\n"))
	lines := collect(t, func(emit func(logLine) error) error { return readMultiplexed(&b, emit) })
	if len(lines) != 2 {
		t.Fatalf("got %+v", lines)
	}
	if lines[1].text != "first half second half" || lines[1].ts.Nanosecond() != 100000000 {
		t.Errorf("joined line: %+v", lines[1])
	}
}

func TestReadsTTYStreams(t *testing.T) {
	raw := "2026-10-19T10:00:00.5Z \x1b[32mINFO\x1b[0m ready\r\n" +
		"2026-10-19T10:00:01Z plain line\r\n" +
		"2026-10-19T10:00:02Z no newline at exit"
	lines := collect(t, func(emit func(logLine) error) error { return readRaw(strings.NewReader(raw), emit) })
	if len(lines) != 3 {
		t.Fatalf("got %+v", lines)
	}
	for _, l := range lines {
		if l.stream != "stdout" || strings.HasSuffix(l.text, "\r") {
			t.Errorf("bad TTY line %+v", l)
		}
	}
	if lines[2].text != "no newline at exit" {
		t.Errorf("trailing line: %q", lines[2].text)
	}
	if sev, _ := parseLevel(lines[0].text); sev != plog.SeverityNumberInfo {
		t.Errorf("coloured level not parsed: %v", sev)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]plog.SeverityNumber{
		`{"level":"error","msg":"db down"}`:                    plog.SeverityNumberError,
		`{"severity":"WARNING","message":"slow"}`:              plog.SeverityNumberWarn,
		`{"level":50,"msg":"pino error"}`:                      plog.SeverityNumberError,
		`{"log":{"level":"debug"},"message":"ecs"}`:            plog.SeverityNumberDebug,
		`{"msg":"no level here"}`:                              plog.SeverityNumberUnspecified,
		"ERROR something broke":                                plog.SeverityNumberError,
		"[WARN] disk almost full":                              plog.SeverityNumberWarn,
		"info: server listening on :3000":                      plog.SeverityNumberInfo,
		"2026-10-19 10:00:00 DEBUG cache miss":                 plog.SeverityNumberDebug,
		"2026-10-19T10:00:00Z FATAL out of memory":             plog.SeverityNumberFatal,
		`time=2026-10-19 level=warn msg="retrying"`:            plog.SeverityNumberWarn,
		"GET /health 200 4ms":                                  plog.SeverityNumberUnspecified,
		"the error count is 0":                                 plog.SeverityNumberUnspecified,
		"Errors: none":                                         plog.SeverityNumberUnspecified,
		"192.168.1.1 - - [19/Oct/2026] \"GET / HTTP/1.1\" 500": plog.SeverityNumberUnspecified,
	}
	for line, want := range cases {
		if got, _ := parseLevel(line); got != want {
			t.Errorf("%q: got %v, want %v", line, got, want)
		}
	}
}

func TestResumeFilterSkipsWhatWasSent(t *testing.T) {
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	f := &resumeFilter{pos: position{TS: at, N: 2}}
	in := []time.Time{at.Add(-time.Second), at, at, at, at.Add(time.Nanosecond)}
	var kept []time.Time
	for _, ts := range in {
		if !f.skip(ts) {
			kept = append(kept, ts)
		}
	}
	// The older line and the two lines already sent at `at` go; the third at `at` and the newer stay.
	if len(kept) != 2 || !kept[0].Equal(at) || !kept[1].Equal(at.Add(time.Nanosecond)) {
		t.Errorf("kept %v", kept)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := t.TempDir() + "/state.json"
	s := loadState(path)
	if _, ok := s.previousRun(); ok {
		t.Fatal("a fresh state should not claim a previous run")
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	s.advance("abc", []time.Time{at.Add(-time.Second), at, at})
	if err := s.save(false); err != nil {
		t.Fatal(err)
	}
	s2 := loadState(path)
	p, ok := s2.get("abc")
	if !ok || !p.TS.Equal(at) || p.N != 2 {
		t.Errorf("position %+v %v", p, ok)
	}
	if _, ok := s2.previousRun(); !ok {
		t.Error("saved state should report a previous run")
	}
	s2.forget("abc")
	if _, ok := s2.get("abc"); ok {
		t.Error("forget kept the container")
	}
}
