// Package zharplogsprocessor applies Zharp's processing rules to every log source: exclude and
// mask rules from the agent config, the built-in masks, and — on Kubernetes — the zharp.io/*
// pod annotations (service, environment, version, source, opt-out, multi-line, per-pod
// exclude and mask). It is the one place these run, whatever receiver the data came from.
package zharplogsprocessor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"github.com/zharp-io/agent/logrules"
)

const typeStr = "zharp_logs"

// NewFactory creates the zharp_logs processor factory.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		processor.WithLogs(createLogs, component.StabilityLevelAlpha),
		processor.WithMetrics(createMetrics, component.StabilityLevelAlpha),
		processor.WithTraces(createTraces, component.StabilityLevelAlpha),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		DefaultMasks: true,
		Kubernetes: KubernetesConfig{
			Multiline:    "auto",
			FlushTimeout: logrules.DefaultIdle,
			MaxLines:     logrules.DefaultMaxLines,
			MaxBytes:     logrules.DefaultMaxBytes,
		},
	}
}

var mutates = processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true})

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	p, err := newLogsProcessor(cfg.(*Config), set.Logger, next)
	if err != nil {
		return nil, err
	}
	return processorhelper.NewLogs(ctx, set, cfg, next, p.processLogs, mutates,
		processorhelper.WithStart(p.start), processorhelper.WithShutdown(p.shutdown))
}

func createMetrics(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	r := newResolver(cfg.(*Config), set.Logger)
	return processorhelper.NewMetrics(ctx, set, cfg, next, r.processMetrics, mutates)
}

func createTraces(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
	r := newResolver(cfg.(*Config), set.Logger)
	return processorhelper.NewTraces(ctx, set, cfg, next, r.processTraces, mutates)
}

func durationOr(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
