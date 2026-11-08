package main

import (
	"runtime"
	"strings"
)

// Programs on the host, for Servers & Containers' "What's using it": the CPU and memory each
// process uses, every 30s. Zharp keeps each server's top programs and drops the rest.
//
// They have their own receiver and pipeline, exporting only to Zharp's OTLP ingest: a host has
// hundreds of processes, and none of their points should take the zharp exporter's path.
//
// A process's command line tells programs apart (two node apps differ only in their script), but
// its arguments can hold anything, passwords and tokens included. transform/processes keeps the
// program and its first two arguments ("node /srv/api/dist/main.js", "java -jar app.jar") and
// drops the rest on the host, before anything is sent.
//
// Linux and Windows only: on macOS the binary is built without cgo, and the process scraper can't
// read most of what it needs there.
const (
	processReceiverID  = "hostmetrics/processes"
	processTransformID = "transform/processes"
	processPipelineID  = "metrics/processes"

	processReceiverBlock = "  " + processReceiverID + ":\n" +
		"    collection_interval: 30s\n" +
		"    scrapers:\n" +
		"      process:\n" +
		// Short-lived processes and ones owned by other users can't always be read; that's normal.
		"        mute_process_name_error: true\n" +
		"        mute_process_exe_error: true\n" +
		"        mute_process_io_error: true\n" +
		"        mute_process_user_error: true\n" +
		"        mute_process_cgroup_error: true\n" +
		"        metrics:\n" +
		"          process.cpu.time:\n            enabled: false\n" +
		"          process.cpu.utilization:\n            enabled: true\n" +
		"          process.memory.usage:\n            enabled: true\n" +
		"          process.memory.virtual:\n            enabled: false\n" +
		"          process.disk.io:\n            enabled: false\n" +
		"        resource_attributes:\n" +
		// The cgroup says when a process belongs to a container, which Zharp already counts.
		"          process.cgroup:\n            enabled: true\n" +
		"          process.command:\n            enabled: false\n" +
		"          process.executable.path:\n            enabled: false\n" +
		"          process.parent_pid:\n            enabled: false\n"

	// The program and its first two arguments, a "quoted path with spaces" counting as one (as
	// Windows writes them). $$ is a literal $ in collector config (confmap), and \" a quote in an
	// OTTL string, so the regex is ^("[^"]*"|[^ ]+)( +("[^"]*"|[^ ]+))?( +("[^"]*"|[^ ]+))?.*$
	// and groups 1, 2 and 4 are kept.
	processTransformBlock = "  " + processTransformID + ":\n" +
		"    error_mode: ignore\n" +
		"    metric_statements:\n" +
		"      - context: resource\n" +
		"        statements:\n" +
		`          - 'replace_pattern(attributes["process.command_line"], "^(\"[^\"]*\"|[^ ]+)( +(\"[^\"]*\"|[^ ]+))?( +(\"[^\"]*\"|[^ ]+))?.*$$", "$$1$$2$$4")'` + "\n"

	processPipelineBlock = "    " + processPipelineID + ":\n" +
		"      receivers: [" + processReceiverID + "]\n" +
		"      processors: [memory_limiter, " + hostProcessorID + ", " + processTransformID + ", batch]\n" +
		"      exporters: [" + otlpExporterID + "]\n"
)

// collectsProcesses reports whether this platform's agent collects processes.
func collectsProcesses() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "windows"
}

// withProcessMetrics adds process collection to a config install wrote, as `update` does for
// every install: the receiver, the transform that trims command lines, and the pipeline. It
// changes nothing in a config that already has it, isn't install's, or is on another platform.
func withProcessMetrics(content string) (string, []string) {
	if !collectsProcesses() || strings.Contains(content, "\n  "+processReceiverID+":") {
		return content, nil
	}
	have := configReceiverIDs(content)
	if !have["hostmetrics"] || !hasPipeline(content, hostPipelineID) ||
		!strings.Contains(content, "\n  "+otlpExporterID+":") || !strings.Contains(content, "\n  "+hostProcessorID+":") {
		return content, nil
	}
	content = strings.Replace(content, "\nprocessors:", "\n"+processReceiverBlock+"\nprocessors:", 1)
	content = strings.Replace(content, "\nprocessors:\n", "\nprocessors:\n"+processTransformBlock, 1)
	content = insertPipeline(content, processPipelineBlock)
	return content, []string{"Processes: CPU and memory per program, for what's using a server's resources (command lines trimmed to the program and its first two arguments)"}
}

// What takes each drive's space, for Servers & Containers' Disk breakdown: hourly, the biggest
// folders two levels down and the space each user owns (zharp_disk_usage, package
// diskusagereceiver). It runs on Linux, in the processes pipeline (OTLP only), through the
// read-only helper install and update put in place (ensureInspectHelper).
const (
	diskUsageReceiverID    = "zharp_disk_usage"
	diskUsageReceiverBlock = "  " + diskUsageReceiverID + ":\n" +
		"    collection_interval: 1h\n"
)

// collectsDiskUsage reports whether this platform's agent measures folders.
func collectsDiskUsage() bool { return runtime.GOOS == "linux" }

// withDiskUsage adds the folder scan to a config that collects processes, as `update` and
// install do. It changes nothing in a config that already has it or doesn't collect processes.
func withDiskUsage(content string) (string, []string) {
	if !collectsDiskUsage() || strings.Contains(content, "\n  "+diskUsageReceiverID+":") || !hasPipeline(content, processPipelineID) {
		return content, nil
	}
	content = strings.Replace(content, "\nprocessors:", "\n"+diskUsageReceiverBlock+"\nprocessors:", 1)
	content = addToPipeline(content, processPipelineID, []string{diskUsageReceiverID})
	return content, []string{"Disk usage: each drive's biggest folders and the space each user owns, hourly"}
}
