package zharpexporter

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap"
)

// rejectedProbeInterval is how often a rejected key is tried again, to notice it working again
// (a key re-enabled, or the workspace restored) without sending on every batch meanwhile.
var rejectedProbeInterval = time.Hour

// errKeyRejected is returned, without a request, while Zharp is rejecting the API key. It is
// permanent so the collector drops the data rather than retrying it.
var errKeyRejected = consumererror.NewPermanent(errors.New("zharpexporter: Zharp rejected the API key — not sending"))

// keyGate stops an API key Zharp has rejected (401/403) from being sent on every batch: one clear
// error, then one probe an hour until it works again. Shared by every exporter using the key —
// logs, metrics, traces and heartbeats back off together.
type keyGate struct {
	mu        sync.Mutex
	rejected  bool
	nextProbe time.Time
	now       func() time.Time
}

var (
	gatesMu sync.Mutex
	gates   = map[string]*keyGate{}
)

// gateFor returns the gate shared by everything sending with apiKey.
func gateFor(apiKey string) *keyGate {
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g, ok := gates[apiKey]
	if !ok {
		g = &keyGate{now: time.Now}
		gates[apiKey] = g
	}
	return g
}

// allow reports whether a request may go out now: always while the key is accepted, and once an
// hour while it's rejected (that request is the probe; others keep waiting).
func (g *keyGate) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.rejected {
		return true
	}
	if g.now().Before(g.nextProbe) {
		return false
	}
	g.nextProbe = g.now().Add(rejectedProbeInterval)
	return true
}

// reject records a 401/403. Only the first one is logged.
func (g *keyGate) reject(status int, logger *zap.Logger) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.rejected {
		logger.Error(fmt.Sprintf("Zharp rejected this API key (HTTP %d) — nothing is being sent. Check ZHARP_API_KEY.", status),
			zap.Duration("retry_every", rejectedProbeInterval))
	}
	g.rejected = true
	g.nextProbe = g.now().Add(rejectedProbeInterval)
}

// accept records a request Zharp took; after a rejection it says once that sending resumed.
func (g *keyGate) accept(logger *zap.Logger) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rejected {
		logger.Info("Zharp accepts the API key again — sending resumed")
	}
	g.rejected = false
}

// send makes one request to Zharp through the key's gate and classifies the outcome:
//   - while the key is rejected: errKeyRejected, without a request;
//   - 401/403: the key is rejected from now on — permanent;
//   - any other 4xx: the payload won't be accepted by sending it again — permanent;
//   - 5xx or a network error: retryable, as before.
func send(client *http.Client, req *http.Request, gate *keyGate, logger *zap.Logger, what string) error {
	if !gate.allow() {
		return errKeyRejected
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("zharpexporter: %s request failed: %w", what, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		gate.reject(resp.StatusCode, logger)
		return errKeyRejected
	case resp.StatusCode >= 500:
		return fmt.Errorf("zharpexporter: %s server returned %d", what, resp.StatusCode)
	case resp.StatusCode >= 400:
		return consumererror.NewPermanent(fmt.Errorf("zharpexporter: %s server returned %d", what, resp.StatusCode))
	}
	gate.accept(logger)
	return nil
}
