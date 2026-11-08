package main

import (
	"strings"
	"testing"
)

// withoutProcesses is a config as install wrote it before agent 1.9.4: no process collection.
func withoutProcesses(content string) string {
	content = strings.Replace(content, processReceiverBlock, "", 1)
	content = strings.Replace(content, processTransformBlock, "", 1)
	return strings.Replace(content, processPipelineBlock, "", 1)
}

func TestInstallCollectsProcesses(t *testing.T) {
	if !collectsProcesses() {
		t.Skip("no process collection on this platform")
	}
	cfg := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true})
	for _, part := range []string{processReceiverBlock, processTransformBlock, processPipelineBlock} {
		if strings.Count(cfg, part) != 1 {
			t.Fatalf("install config should have this once:\n%s\n---\n%s", part, cfg)
		}
	}
}

func TestUpdateAddsProcessesOnce(t *testing.T) {
	if !collectsProcesses() {
		t.Skip("no process collection on this platform")
	}
	old := withoutProcesses(buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true}))
	if strings.Contains(old, processReceiverID) {
		t.Fatal("test setup: the old config still collects processes")
	}

	updated, notes := withProcessMetrics(old)
	if len(notes) != 1 {
		t.Fatalf("expected one note, got %v", notes)
	}
	for _, part := range []string{processReceiverBlock, processTransformBlock, processPipelineBlock} {
		if strings.Count(updated, part) != 1 {
			t.Fatalf("updated config should have this once:\n%s\n---\n%s", part, updated)
		}
	}
	// The receiver sits among the receivers and the transform among the processors.
	receivers := updated[strings.Index(updated, "receivers:\n"):strings.Index(updated, "\nprocessors:\n")]
	if !strings.Contains(receivers, processReceiverID+":") {
		t.Fatal("the process receiver isn't under receivers")
	}
	processors := updated[strings.Index(updated, "\nprocessors:\n"):strings.Index(updated, "\nexporters:\n")]
	if !strings.Contains(processors, processTransformID+":") {
		t.Fatal("the transform isn't under processors")
	}

	again, notes := withProcessMetrics(updated)
	if again != updated || notes != nil {
		t.Fatal("a second update should change nothing")
	}
}

func TestUpdateLeavesOtherConfigsAlone(t *testing.T) {
	foreign := "receivers:\n  otlp:\n    protocols:\n      grpc:\n\nprocessors:\n  batch:\n\nexporters:\n  debug:\n\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [debug]\n"
	if got, notes := withProcessMetrics(foreign); got != foreign || notes != nil {
		t.Fatal("a config install didn't write should be left as it is")
	}
}

func TestCommandLinesAreTrimmedOnTheHost(t *testing.T) {
	// The OTTL regex as the collector receives it ($$ is confmap's escaped $).
	if !strings.Contains(processTransformBlock, `replace_pattern(attributes["process.command_line"]`) ||
		!strings.Contains(processTransformBlock, `"$$1$$2$$4"`) {
		t.Fatalf("the command line must be trimmed before it's sent:\n%s", processTransformBlock)
	}
	if !strings.Contains(processPipelineBlock, processTransformID) || !strings.Contains(processPipelineBlock, "exporters: ["+otlpExporterID+"]\n") {
		t.Fatalf("processes go through the transform, and only to the OTLP exporter:\n%s", processPipelineBlock)
	}
}
