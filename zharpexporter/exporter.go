package zharpexporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// AgentVersion is set at build time via -ldflags from dist/main.go.
var AgentVersion = "dev"

type logEntry struct {
	Timestamp string         `json:"timestamp"`
	Service   string         `json:"service"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	// Unified service tags. Omitted when unknown so older backends see the payload they always did.
	Env     string `json:"env,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
}

type ingestPayload struct {
	Hostname string     `json:"hostname"`
	OS       string     `json:"os"`
	Version  string     `json:"version"`
	Logs     []logEntry `json:"logs"`
}

type zharpLogsExporter struct {
	cfg      *Config
	logger   *zap.Logger
	client   *http.Client
	hostname string
}

// shared helpers used by both logs and metrics exporters
var hostnameFunc = os.Hostname

func defaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func newRequest(ctx context.Context, url string, body io.Reader, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("zharpexporter: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", apiKey)
	return req, nil
}

func newLogsExporter(cfg *Config, logger *zap.Logger) *zharpLogsExporter {
	hostname, _ := hostnameFunc()
	return &zharpLogsExporter{
		cfg:      cfg,
		logger:   logger,
		client:   defaultHTTPClient(cfg.Timeout),
		hostname: hostname,
	}
}

// Capabilities implements consumer.BaseConsumer.
func (e *zharpLogsExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// Start implements component.Component.
func (e *zharpLogsExporter) Start(_ context.Context, _ component.Host) error {
	e.logger.Info("Zharp exporter started", zap.String("endpoint", e.cfg.baseURL()))
	return nil
}

// Shutdown implements component.Component.
func (e *zharpLogsExporter) Shutdown(_ context.Context) error {
	return nil
}

// ConsumeLogs implements exporter.Logs.
func (e *zharpLogsExporter) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	entries := e.convertLogs(ld)
	if len(entries) == 0 {
		return nil
	}
	for i := 0; i < len(entries); i += e.cfg.BatchSize {
		end := i + e.cfg.BatchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := e.ship(ctx, entries[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (e *zharpLogsExporter) convertLogs(ld plog.Logs) []logEntry {
	var entries []logEntry

	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)

		resource := rl.Resource().Attributes()

		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				lr := sl.LogRecords().At(k)

				// Prefer the log record's own timestamp (set by regex_parser operator).
				// Fall back to observed timestamp (when the file line was read).
				// lr.Timestamp() == 0 means the operator never set it; AsTime() would
				// return Unix epoch (1970) which IsZero() does NOT catch.
				var ts time.Time
				switch {
				case lr.Timestamp() != 0:
					ts = lr.Timestamp().AsTime()
				case lr.ObservedTimestamp() != 0:
					ts = lr.ObservedTimestamp().AsTime()
				default:
					ts = time.Now().UTC()
				}

				// Collect log record attributes as metadata
				var meta map[string]any
				if lr.Attributes().Len() > 0 {
					meta = make(map[string]any, lr.Attributes().Len())
					lr.Attributes().Range(func(k string, v pcommon.Value) bool {
						meta[k] = v.AsString()
						return true
					})
				}

				tags := resolveTags(resource, lr.Attributes())
				serviceName := tags.Service
				if serviceName == "" {
					serviceName = "unknown"
				}

				entries = append(entries, logEntry{
					Timestamp: ts.UTC().Format(time.RFC3339Nano),
					Service:   serviceName,
					Level:     severityToLevel(lr.SeverityNumber()),
					Message:   lr.Body().AsString(),
					Metadata:  meta,
					Env:       tags.Env,
					Version:   tags.Version,
					Source:    tags.Source,
				})
			}
		}
	}

	return entries
}

func (e *zharpLogsExporter) ship(ctx context.Context, batch []logEntry) error {
	p := ingestPayload{
		Hostname: e.hostname,
		OS:       runtime.GOOS,
		Version:  AgentVersion,
		Logs:     batch,
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("zharpexporter: marshal failed: %w", err)
	}

	url := fmt.Sprintf("%s/agent/logs/%s", e.cfg.baseURL(), e.cfg.APIKey)
	req, err := newRequest(ctx, url, bytes.NewReader(body), e.cfg.APIKey)
	if err != nil {
		return err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("zharpexporter: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("zharpexporter: server returned %d", resp.StatusCode)
	}

	e.logger.Debug("shipped logs to Zharp", zap.Int("count", len(batch)))
	return nil
}

func severityToLevel(sn plog.SeverityNumber) string {
	switch {
	case sn >= plog.SeverityNumberFatal:
		return "fatal"
	case sn >= plog.SeverityNumberError:
		return "error"
	case sn >= plog.SeverityNumberWarn:
		return "warn"
	case sn >= plog.SeverityNumberInfo:
		return "info"
	case sn >= plog.SeverityNumberDebug:
		return "debug"
	default:
		// SeverityNumberUnspecified — syslog/filelog lines with no OTel severity.
		return "info"
	}
}
