package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/zharp-io/agent/logrules"
)

func TestInstallRulesInGeneratedConfig(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = ""; installRules = logRulesSpec{} }()
	installRules = logRulesSpec{Rules: []logrules.Rule{
		{Type: "exclude", Pattern: `GET /health(z)?$`},
		{Type: "mask", Pattern: `(api_key=)\w+`, Replace: "${1}***"},
	}}
	content := buildConfig(configSpec{
		metricsReceivers: []string{"hostmetrics"},
		logSources:       []LogSource{{Name: "myapp", Paths: []string{"/var/log/myapp/*.log"}}},
		logReceivers:     []LogReceiver{dockerLogsReceiver()},
	})
	if !strings.Contains(content, "processors: [memory_limiter, zharp_logs, resourcedetection, batch]") {
		t.Fatalf("logs pipeline:\n%s", content)
	}
	if !strings.Contains(content, `pattern: 'GET /health(z)?$$'`) || !strings.Contains(content, `replace: '$${1}***'`) {
		t.Fatalf("$ not escaped for the collector:\n%s", content)
	}
	if !strings.Contains(content, "type: recombine") {
		t.Fatal("file source without multi-line joining")
	}
	validateContent(t, content, runtime.GOOS == "linux")

	spec, err := readLogRules(content)
	if err != nil || len(spec.Rules) != 2 || spec.Rules[0].Pattern != `GET /health(z)?$` || spec.Rules[1].Replace != "${1}***" || spec.NoDefaultMasks {
		t.Fatalf("read back %+v %v", spec, err)
	}
}

func TestAddRuleToAnOlderConfig(t *testing.T) {
	stateDirOverride = t.TempDir()
	defer func() { stateDirOverride = "" }()
	// A config written before zharp_logs existed.
	old := buildConfig(configSpec{metricsReceivers: []string{"hostmetrics"}, logReceivers: []LogReceiver{dockerLogsReceiver()}})
	old = removeReceiverBlock(old, rulesProcessor)
	old = strings.Replace(old, "[memory_limiter, zharp_logs, resourcedetection, batch]", "[memory_limiter, resourcedetection, batch]", 1)
	if strings.Contains(old, rulesProcessor) {
		t.Fatal("test setup")
	}
	content := ensureLogRules(old)
	if !strings.Contains(content, "\n  zharp_logs:\n    # Built-in masks") || !strings.Contains(content, "[memory_limiter, zharp_logs, resourcedetection, batch]") {
		t.Fatalf("ensureLogRules:\n%s", content)
	}
	if again := ensureLogRules(content); again != content {
		t.Fatal("ensureLogRules is not idempotent")
	}

	rule, _, err := parseRuleFlags("mask", []string{"--pattern", `\b\d{3}-\d{2}-\d{4}\b`})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := readLogRules(content)
	spec.Rules = append(spec.Rules, rule)
	_, on, _ := parseRuleFlags("default-masks", []string{"--off"})
	spec.NoDefaultMasks = !*on
	content = writeLogRules(content, spec)
	if strings.Count(content, "\n  zharp_logs:\n") != 1 || !strings.Contains(content, "default_masks: false") {
		t.Fatalf("writeLogRules:\n%s", content)
	}
	validateContent(t, content, true)

	if _, _, err := parseRuleFlags("mask", []string{"--pattern", "("}); err == nil {
		t.Error("bad regex accepted")
	}
	if _, _, err := parseRuleFlags("exclude", []string{"--pattern", "x", "--replace", "y"}); err == nil {
		t.Error("--replace accepted on exclude")
	}
	if _, _, err := parseRuleFlags("drop", []string{"--pattern", "x"}); err == nil {
		t.Error("unknown kind accepted")
	}
}
