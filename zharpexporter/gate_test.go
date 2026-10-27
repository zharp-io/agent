package zharpexporter

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A server answering every request with the status in *status, counting them.
func statusServer(t *testing.T, status *atomic.Int32, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRejectedKeyBacksOffAndProbesHourly(t *testing.T) {
	var status, hits atomic.Int32
	status.Store(http.StatusUnauthorized)
	srv := statusServer(t, &status, &hits)

	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	gate := &keyGate{now: func() time.Time { return now }}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	// The first 401 is sent, logged once, and permanent.
	err := send(srv.Client(), post(t, srv.URL), gate, logger, "logs")
	if !consumererror.IsPermanent(err) {
		t.Fatalf("401 should be permanent, got %v", err)
	}
	// Further batches are dropped without a request, and without more log lines.
	for range 5 {
		if err := send(srv.Client(), post(t, srv.URL), gate, logger, "logs"); !consumererror.IsPermanent(err) {
			t.Fatalf("while rejected: want permanent, got %v", err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("want 1 request while rejected, got %d", hits.Load())
	}
	if n := logs.FilterMessageSnippet("Zharp rejected this API key (HTTP 401)").Len(); n != 1 {
		t.Fatalf("want the rejection logged once, got %d", n)
	}

	// An hour on, one probe goes out; the key works again and sending resumes, said once.
	now = now.Add(rejectedProbeInterval)
	status.Store(http.StatusOK)
	if err := send(srv.Client(), post(t, srv.URL), gate, logger, "logs"); err != nil {
		t.Fatalf("probe after the key works: %v", err)
	}
	if err := send(srv.Client(), post(t, srv.URL), gate, logger, "logs"); err != nil {
		t.Fatalf("after resuming: %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("want probe + one batch after resuming, got %d requests", hits.Load())
	}
	if n := logs.FilterMessageSnippet("sending resumed").Len(); n != 1 {
		t.Fatalf("want the resume logged once, got %d", n)
	}
}

func TestForbiddenAlsoBacksOff(t *testing.T) {
	var status, hits atomic.Int32
	status.Store(http.StatusForbidden)
	srv := statusServer(t, &status, &hits)
	gate := &keyGate{now: time.Now}
	for range 3 {
		_ = send(srv.Client(), post(t, srv.URL), gate, zap.NewNop(), "metrics")
	}
	if hits.Load() != 1 {
		t.Fatalf("403 should back off like 401, got %d requests", hits.Load())
	}
}

func TestOther4xxIsPermanentWithoutBackingOff(t *testing.T) {
	var status, hits atomic.Int32
	status.Store(http.StatusBadRequest)
	srv := statusServer(t, &status, &hits)
	gate := &keyGate{now: time.Now}
	for range 2 {
		if err := send(srv.Client(), post(t, srv.URL), gate, zap.NewNop(), "logs"); !consumererror.IsPermanent(err) {
			t.Fatalf("400 should be permanent, got %v", err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("a bad payload doesn't stop the key: want 2 requests, got %d", hits.Load())
	}
}

func TestServerErrorStaysRetryable(t *testing.T) {
	var status, hits atomic.Int32
	status.Store(http.StatusBadGateway)
	srv := statusServer(t, &status, &hits)
	err := send(srv.Client(), post(t, srv.URL), &keyGate{now: time.Now}, zap.NewNop(), "logs")
	if err == nil || consumererror.IsPermanent(err) {
		t.Fatalf("5xx should be retryable, got %v", err)
	}
}

func TestSignalsShareTheKeysGate(t *testing.T) {
	if gateFor("key-a") != gateFor("key-a") {
		t.Fatal("logs, metrics and traces with one key should share a gate")
	}
	if gateFor("key-a") == gateFor("key-b") {
		t.Fatal("different keys need their own gates")
	}
}
