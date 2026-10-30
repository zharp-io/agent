package zharpexporter

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter"
)

const typeStr = "zharp"

// NewFactory creates the Zharp exporter factory.
//
// It carries the agent's own logs, database and broker metrics, and its heartbeat to the
// /agent/* endpoints. Applications' OpenTelemetry and the host and container metrics go to
// Zharp's OTLP ingest through the otlphttp exporter instead.
func NewFactory() exporter.Factory {
	return exporter.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		exporter.WithLogs(createLogsExporter, component.StabilityLevelBeta),
		exporter.WithMetrics(createMetricsExporter, component.StabilityLevelBeta),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		Timeout:   10 * time.Second,
		BatchSize: 500,
	}
}


func createLogsExporter(
	ctx context.Context,
	set exporter.Settings,
	cfg component.Config,
) (exporter.Logs, error) {
	c := cfg.(*Config)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return newLogsExporter(c, set.Logger), nil
}

func createMetricsExporter(
	ctx context.Context,
	set exporter.Settings,
	cfg component.Config,
) (exporter.Metrics, error) {
	c := cfg.(*Config)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return newMetricsExporter(c, set.Logger), nil
}
