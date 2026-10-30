package main

import (
	"strings"
	"testing"
)

func TestOTLPEndpointFor(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "https://api.zharp.io/api/v1/otlp",
		"http://localhost:3000/api/v1":   "http://localhost:3000/api/v1/otlp",
		" https://zharp.example.com/ ":   "https://zharp.example.com/otlp",
		"https://api.zharp.io/api/v1///": "https://api.zharp.io/api/v1/otlp",
	} {
		if got := otlpEndpointFor(in); got != want {
			t.Errorf("otlpEndpointFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// A fresh install receives OTLP on localhost and forwards traces and logs to the OTLP ingest of
// the API it was given, with the agent's key; the agent's own logs and metrics are untouched.
func TestGeneratedConfigForwardsOTLP(t *testing.T) {
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logSources:       []LogSource{{Name: "myapp", Paths: []string{"/srv/myapp/*.log"}}},
		tracing:          true,
		endpoint:         "http://localhost:3000/api/v1",
	})
	for _, want := range []string{
		"        endpoint: 127.0.0.1:4317\n",
		"        endpoint: 127.0.0.1:4318\n",
		"  resourcedetection/otlp:\n    detectors: [system]\n    timeout: 5s\n    override: false\n",
		"  otlp_http/zharp:\n    endpoint: \"http://localhost:3000/api/v1/otlp\"\n    headers:\n      Authorization: \"Bearer ${env:ZHARP_API_KEY}\"\n",
		"    traces/otlp:\n      receivers: [otlp]\n      processors: [memory_limiter, resourcedetection/otlp, batch]\n      exporters: [otlp_http/zharp]\n",
		"    logs/otlp:\n      receivers: [otlp]\n      processors: [memory_limiter, resourcedetection/otlp, batch]\n      exporters: [otlp_http/zharp]\n",
		"    logs:\n      receivers: [filelog/myapp]\n      processors: [memory_limiter, zharp_logs, resourcedetection, batch]\n      exporters: [zharp]\n",
		"    metrics:\n      receivers: [hostmetrics]\n      processors: [memory_limiter, resourcedetection, batch]\n      exporters: [zharp]\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in\n%s", want, content)
		}
	}
	validateMerged(t, content)
}

func TestNoOTLPLeavesItOutAndUpdateKeepsItOut(t *testing.T) {
	fakeHost(t)
	content := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true, noOTLP: true})
	if !strings.HasPrefix(content, "## Zharp Collector config") || !strings.Contains(content, noOTLPMarker) {
		t.Fatalf("no --no-otlp marker:\n%s", content)
	}
	for _, unwanted := range []string{"\n  otlp:", "otlphttp", "traces/otlp", "logs/otlp"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("--no-otlp config has %q", unwanted)
		}
	}
	res, err := mergeSources(content, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != content {
		t.Errorf("update changed a --no-otlp config:\n%s", res.config)
	}
}

// legacyConfig is an install from before the passthrough: OTLP spans through the zharp exporter.
func legacyConfig(t *testing.T) string {
	t.Helper()
	content := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, tracing: true, endpoint: "https://zharp.example.com"})
	content = strings.Replace(content, otlpProcessorBlock, "", 1)
	content = strings.Replace(content, otlpExporterBlock("https://zharp.example.com", apiKeyRef), "", 1)
	content = strings.Replace(content, otlpPipelinesBlock, legacyTracesPipelineBlock, 1)
	if !strings.Contains(content, legacyTracesPipelineBlock) || strings.Contains(content, "otlphttp") {
		t.Fatalf("not a legacy config:\n%s", content)
	}
	return content
}

func TestMergeMovesLegacyTracesToOTLPPassthrough(t *testing.T) {
	fakeHost(t)
	before := legacyConfig(t)
	res, err := mergeSources(before, []InventoryItem{apmInventoryItem()}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	after := res.config
	if strings.Contains(after, legacyTracesPipelineBlock) || strings.Count(after, "    traces/otlp:\n") != 1 || strings.Count(after, "\n  otlp:\n") != 1 {
		t.Errorf("legacy traces pipeline not replaced exactly once:\n%s", after)
	}
	for _, want := range []string{
		"    endpoint: \"https://zharp.example.com/otlp\"\n",
		"    logs/otlp:\n",
		"\n  resourcedetection/otlp:\n",
		"    endpoint: \"https://zharp.example.com\"\n", // the zharp exporter's own, kept
	} {
		if !strings.Contains(after, want) {
			t.Errorf("missing %q in\n%s", want, after)
		}
	}
	validateMerged(t, after)

	// Running it again changes nothing.
	again, err := mergeSources(after, res.inventory, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.config != after {
		t.Errorf("second update changed the config:\n%s", again.config)
	}
}

// A traces pipeline someone wrote is theirs: update doesn't add a second one next to it.
func TestMergeLeavesCustomTracesPipeline(t *testing.T) {
	fakeHost(t)
	custom := strings.Replace(legacyConfig(t), "      processors: [memory_limiter, batch]\n      exporters: [zharp]\n",
		"      processors: [memory_limiter, resourcedetection, batch]\n      exporters: [zharp]\n", 1)
	res, err := mergeSources(custom, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != custom {
		t.Errorf("custom traces pipeline config changed:\n%s", res.config)
	}
	if len(res.notAdded) != 1 || !strings.Contains(res.notAdded[0], "own traces pipeline") {
		t.Errorf("notAdded = %v", res.notAdded)
	}
}
