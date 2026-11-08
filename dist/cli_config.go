package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// installConfigDir returns the platform-appropriate config directory.
func installConfigDir() string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("PROGRAMDATA")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\zharp-collector`
	}
	return "/etc/zharp-collector"
}

// installConfigFile returns the full path to the config YAML.
func installConfigFile() string {
	if runtime.GOOS == "windows" {
		return installConfigDir() + `\config.yaml`
	}
	return installConfigDir() + "/config.yaml"
}

// installEnvFile returns the full path to the env/secrets file.
func installEnvFile() string {
	if runtime.GOOS == "windows" {
		return installConfigDir() + `\env.ps1`
	}
	return installConfigDir() + "/env"
}

// LogSource represents a named log source — a service name and the file paths
// that belong to it. The service name becomes resource["service.name"] in the
// dashboard, so logs from each source are grouped under their service.
type LogSource struct {
	Name  string
	Paths []string
}

// buildConfigYAML generates the full OTel collector YAML configuration.
// metricsReceivers is the list of metric receiver names (e.g. ["hostmetrics", "postgresql"]).
// receiverBlocks contains the raw YAML blocks for each additional metric receiver.
// logSources is the list of named log sources (may be empty).
// tracing turns on the OTLP receiver and the traces/otlp and logs/otlp pipelines, for
// applications instrumented with an OpenTelemetry SDK that report to this collector.
// endpoint overrides the Zharp API the exporter sends to; empty keeps the default.
func buildConfigYAML(metricsReceivers []string, receiverBlocks []string, logSources []LogSource, tracing bool, endpoint string) string {
	return buildConfig(configSpec{metricsReceivers: metricsReceivers, receiverBlocks: receiverBlocks, logSources: logSources, tracing: tracing, endpoint: endpoint})
}

// configSpec is everything a generated config holds beyond its fixed parts.
type configSpec struct {
	metricsReceivers []string      // names for the metrics pipeline, e.g. hostmetrics, postgresql
	receiverBlocks   []string      // raw YAML blocks for the metric receivers
	logSources       []LogSource   // files tailed by filelog/<name>
	logReceivers     []LogReceiver // other log receivers: docker_logs, journald, filelog/pm2
	tracing          bool          // the OTLP receiver and the pipelines forwarding it to Zharp's OTLP ingest
	endpoint         string
	noAppLogs        bool // installed with --no-app-logs: noted in the header so `update` keeps them out
	noOTLP           bool // installed with --no-otlp: noted in the header so `update` keeps it out
}

// The OTLP receiver applications on this host send to, and the pipelines that forward what it
// receives to Zharp's OTLP ingest unchanged.
//
// Traces and logs from an OpenTelemetry SDK go out as OTLP through the standard otlphttp
// exporter, not through the zharp exporter's JSON: the SDK already speaks the format Zharp's
// application monitoring reads, so the agent only adds the host's attributes, batches and
// retries. The agent's own logs (files, containers, the journal) and database metrics keep
// going to the /agent/* endpoints through the zharp exporter.
const (
	otlpReceiverBlock = "  otlp:\n" +
		"    protocols:\n" +
		"      grpc:\n" +
		"        endpoint: 127.0.0.1:4317\n" +
		"      http:\n" +
		"        endpoint: 127.0.0.1:4318\n"

	// otlpProcessorID adds host.name and os.type. override: false keeps what the application
	// set itself — service.name above all, which the env detector could otherwise replace with
	// the collector's own.
	otlpProcessorID    = "resourcedetection/otlp"
	otlpProcessorBlock = "  " + otlpProcessorID + ":\n" +
		"    detectors: [system]\n" +
		"    timeout: 5s\n" +
		"    override: false\n"

	otlpExporterID = "otlp_http/zharp"

	otlpPipelinesBlock = "    traces/otlp:\n      receivers: [otlp]\n" +
		"      processors: [memory_limiter, " + otlpProcessorID + ", batch]\n" +
		"      exporters: [" + otlpExporterID + "]\n" +
		"    logs/otlp:\n      receivers: [otlp]\n" +
		"      processors: [memory_limiter, " + otlpProcessorID + ", batch]\n" +
		"      exporters: [" + otlpExporterID + "]\n"

	// legacyTracesPipelineBlock is the traces pipeline installs wrote before the OTLP
	// passthrough: spans through the zharp exporter to /agent/traces, which Zharp no longer
	// has (nor does the exporter). `update` removes it; traces/otlp takes its place.
	legacyTracesPipelineBlock = "    traces:\n      receivers: [otlp]\n" +
		"      processors: [memory_limiter, batch]\n" +
		"      exporters: [zharp]\n"
)

// Host and container metrics (Servers & Containers) go to Zharp's OTLP ingest as OTLP metrics:
// the metrics/host pipeline reads the host (hostmetrics), Docker (docker_stats for usage,
// docker_state for state, exit codes and restarts) and, in Kubernetes, the kubelet, and exports
// through otlp_http/zharp. The zharp exporter is listed too only for its heartbeat, which
// carries the agent's inventory; it doesn't send these metrics (zharpexporter drops them).
// Databases and brokers stay in the metrics pipeline, to the zharp exporter.
const (
	hostPipelineID    = "metrics/host"
	hostProcessorID   = "resourcedetection/host"
	dockerReceiverIDs = "docker_stats,docker_state"

	// hostmetricsBlock is what Servers & Containers reads: CPU and memory use, the core count
	// and memory total, load, filesystem use per mount, network bytes and uptime. Volume is
	// kept down by turning off what nothing reads (CPU time, inodes, packet counts).
	hostmetricsBlock = "  hostmetrics:\n" +
		"    collection_interval: 30s\n" +
		"    scrapers:\n" +
		"      cpu:\n" +
		"        metrics:\n" +
		"          system.cpu.time:\n            enabled: false\n" +
		"          system.cpu.utilization:\n            enabled: true\n" +
		"          system.cpu.logical.count:\n            enabled: true\n" +
		"      memory:\n" +
		"        metrics:\n" +
		"          system.memory.utilization:\n            enabled: true\n" +
		"          system.memory.limit:\n            enabled: true\n" +
		"      load:\n" +
		"      filesystem:\n" +
		"        exclude_fs_types:\n" +
		"          fs_types: [autofs, binfmt_misc, bpf, cgroup, cgroup2, configfs, debugfs, devpts, devtmpfs, fusectl, hugetlbfs, iso9660, mqueue, nsfs, overlay, proc, procfs, pstore, rpc_pipefs, securityfs, selinuxfs, squashfs, sysfs, tmpfs, tracefs]\n" +
		"          match_type: strict\n" +
		"        exclude_mount_points:\n" +
		"          mount_points: [\"/dev/.*\", \"/proc/.*\", \"/sys/.*\", \"/run/.*\", \"/snap/.*\", \"/var/lib/docker/.*\", \"/var/lib/kubelet/.*\", \"/var/lib/containers/.*\", \"/boot/efi\"]\n" +
		"          match_type: regexp\n" +
		"        metrics:\n" +
		"          system.filesystem.inodes.usage:\n            enabled: false\n" +
		"      network:\n" +
		"        metrics:\n" +
		"          system.network.connections:\n            enabled: false\n" +
		"          system.network.packets:\n            enabled: false\n" +
		"          system.network.dropped:\n            enabled: false\n" +
		"          system.network.errors:\n            enabled: false\n" +
		"      system:\n"

	// hostProcessorBlock names the server: host.name as the OS reports it (what the heartbeat
	// uses too), the OS, addresses and, on a cloud VM, provider and region. env carries
	// zharp.agent.version, which the binary puts in OTEL_RESOURCE_ATTRIBUTES. Off-cloud the
	// cloud detectors find no metadata endpoint and add nothing.
	hostProcessorBlock = "  " + hostProcessorID + ":\n" +
		"    detectors: [env, system, ec2, gcp, azure]\n" +
		"    timeout: 5s\n" +
		"    system:\n" +
		"      hostname_sources: [os]\n" +
		"      resource_attributes:\n" +
		"        os.name:\n          enabled: true\n" +
		"        os.version:\n          enabled: true\n" +
		"        os.description:\n          enabled: true\n" +
		"        host.ip:\n          enabled: true\n" +
		"        host.arch:\n          enabled: true\n"
)

// isHostReceiver reports a receiver whose metrics belong in metrics/host.
func isHostReceiver(id string) bool {
	base, _, _ := strings.Cut(strings.TrimSpace(id), "/")
	switch base {
	case "hostmetrics", "docker_stats", "docker_state", "kubeletstats":
		return true
	}
	return false
}

// splitMetricReceivers splits receiver ids (some comma-joined, like dockerReceiverIDs) into
// the host pipeline's and the metrics pipeline's.
func splitMetricReceivers(ids []string) (host, other []string) {
	for _, joined := range ids {
		for _, id := range strings.Split(joined, ",") {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			if isHostReceiver(id) {
				host = append(host, id)
			} else {
				other = append(other, id)
			}
		}
	}
	return host, other
}

// hostPipelineBlock is the metrics/host pipeline for ids.
func hostPipelineBlock(ids []string) string {
	return "    " + hostPipelineID + ":\n" +
		"      receivers: [" + strings.Join(ids, ", ") + "]\n" +
		"      processors: [memory_limiter, " + hostProcessorID + ", batch]\n" +
		"      exporters: [" + otlpExporterID + ", zharp]\n"
}

// metricsPipelineBlock is the pipeline of database and broker receivers, to the zharp exporter.
func metricsPipelineBlock(ids []string) string {
	return "    metrics:\n" +
		"      receivers: [" + strings.Join(ids, ", ") + "]\n" +
		"      processors: [memory_limiter, resourcedetection, batch]\n" +
		"      exporters: [zharp]\n"
}

// dockerReceiverBlocks are docker_stats (usage) and docker_state (state, exit codes, restarts,
// ports), both on the Docker socket.
func dockerReceiverBlocks() string {
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	return fmt.Sprintf("\n  docker_stats:\n    endpoint: %s\n    collection_interval: 30s\n    timeout: 20s\n"+
		"  docker_state:\n    endpoint: %s\n    collection_interval: 30s\n", endpoint, endpoint)
}

// defaultAPIEndpoint is the Zharp API the zharp exporter uses when the config names none.
const defaultAPIEndpoint = "https://api.zharp.io/api/v1"

// otlpEndpointFor is Zharp's OTLP ingest for the API the agent is configured with
// (https://api.zharp.io/api/v1 → https://api.zharp.io/api/v1/otlp). otlphttp appends
// /v1/traces and /v1/logs to it.
func otlpEndpointFor(api string) string {
	api = strings.TrimRight(strings.TrimSpace(api), "/")
	if api == "" {
		api = defaultAPIEndpoint
	}
	return api + "/otlp"
}

// apiKeyRef is how generated configs read the API key: from the service's environment.
const apiKeyRef = "${env:ZHARP_API_KEY}"

// otlpExporterBlock is the otlphttp exporter forwarding to Zharp's OTLP ingest, authenticated
// with the agent's own API key. Batches are gzipped, held in a queue and retried with backoff
// while Zharp is unreachable or answers 429/503.
func otlpExporterBlock(api, apiKey string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  %s:\n", otlpExporterID)
	fmt.Fprintf(&sb, "    endpoint: %q\n", otlpEndpointFor(api))
	sb.WriteString("    headers:\n")
	fmt.Fprintf(&sb, "      Authorization: %q\n", "Bearer "+apiKey)
	sb.WriteString("    compression: gzip\n")
	sb.WriteString("    retry_on_failure:\n")
	sb.WriteString("      enabled: true\n")
	sb.WriteString("    sending_queue:\n")
	sb.WriteString("      enabled: true\n")
	return sb.String()
}

// noAppLogsMarker is the header line of a config installed with --no-app-logs.
const noAppLogsMarker = "## Application logs left out (--no-app-logs); `zharp-collector update` keeps them out.\n"

// noOTLPMarker is the header line of a config installed with --no-otlp.
const noOTLPMarker = "## OTLP receiver left out (--no-otlp); `zharp-collector update` keeps it out.\n"

// buildConfig generates the collector config for spec.
func buildConfig(spec configSpec) string {
	metricsReceivers, receiverBlocks, logSources, endpoint := spec.metricsReceivers, spec.receiverBlocks, spec.logSources, spec.endpoint
	tracing := spec.tracing && !spec.noOTLP
	storage := false
	for _, r := range spec.logReceivers {
		storage = storage || r.Storage
	}

	var sb strings.Builder

	sb.WriteString("## Zharp Collector config — generated by zharp-collector install\n")
	sb.WriteString("## Edit this file then restart the service to apply changes.\n")
	if spec.noAppLogs {
		sb.WriteString(noAppLogsMarker)
	}
	if spec.noOTLP {
		sb.WriteString(noOTLPMarker)
	}
	sb.WriteString("\n")

	sb.WriteString("extensions:\n")
	sb.WriteString("  health_check:\n")
	sb.WriteString("    endpoint: 0.0.0.0:13133\n")
	if storage {
		sb.WriteString(storageExtensionBlock())
	}
	sb.WriteString("\n")

	sb.WriteString("receivers:\n")
	sb.WriteString(hostmetricsBlock)
	if collectsProcesses() {
		sb.WriteString(processReceiverBlock)
	}

	// One filelog/<name> receiver per log source, each stamped with its service name.
	for _, src := range logSources {
		fmt.Fprintf(&sb, "  filelog/%s:\n", src.Name)
		sb.WriteString("    include:\n")
		for _, p := range src.Paths {
			fmt.Fprintf(&sb, "      - %s\n", p)
		}
		sb.WriteString("    include_file_path: true\n")
		sb.WriteString("    include_file_name: false\n")
		sb.WriteString(filelogOperatorsBlock(src.Name))
	}

	// Application logs that aren't plain files of one service: containers, the journal, PM2.
	for _, r := range spec.logReceivers {
		sb.WriteString(r.Block)
	}

	// OTLP receiver — the port applications send their traces and logs to. Bound to
	// localhost by default because a trace carries request URLs, query text and error messages;
	// opening it to the network is a decision to make deliberately, not one to inherit from an installer.
	if tracing {
		sb.WriteString(otlpReceiverBlock)
	}

	// Additional metric receiver blocks.
	for _, block := range receiverBlocks {
		sb.WriteString(block)
		if !strings.HasSuffix(block, "\n") {
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\nprocessors:\n")
	sb.WriteString("  batch:\n")
	sb.WriteString("    send_batch_size: 500\n")
	sb.WriteString("    timeout: 5s\n")
	sb.WriteString("  memory_limiter:\n")
	sb.WriteString("    limit_mib: 128\n")
	sb.WriteString("    spike_limit_mib: 32\n")
	sb.WriteString("    check_interval: 5s\n")
	sb.WriteString("  resourcedetection:\n")
	sb.WriteString("    detectors: [system, env]\n")
	sb.WriteString("    timeout: 5s\n")
	sb.WriteString(hostProcessorBlock)
	if collectsProcesses() {
		sb.WriteString(processTransformBlock)
	}
	if tracing {
		sb.WriteString(otlpProcessorBlock)
	}
	// Exclude/mask rules and the built-in masks, for every log source.
	sb.WriteString(rulesProcessorBlock(installRules))

	sb.WriteString("\nexporters:\n")
	sb.WriteString("  zharp:\n")
	sb.WriteString("    api_key: \"${env:ZHARP_API_KEY}\"\n")
	if endpoint != "" {
		fmt.Fprintf(&sb, "    endpoint: %q\n", endpoint)
	}
	// What this host was found running, sent with each heartbeat so the dashboard can show it.
	fmt.Fprintf(&sb, "    inventory_file: %q\n", installInventoryFile())
	// Host and container metrics always go to the OTLP ingest, so the exporter is always there;
	// --no-otlp only leaves out the receiver applications send to.
	sb.WriteString(otlpExporterBlock(endpoint, apiKeyRef))

	sb.WriteString("\nservice:\n")
	if storage {
		fmt.Fprintf(&sb, "  extensions: [health_check, %s]\n", storageExtension)
	} else {
		sb.WriteString("  extensions: [health_check]\n")
	}
	sb.WriteString("  pipelines:\n")

	// Logs pipeline — one receiver per source.
	if len(logSources) > 0 || len(spec.logReceivers) > 0 {
		var logReceivers []string
		for _, src := range logSources {
			logReceivers = append(logReceivers, "filelog/"+src.Name)
		}
		for _, r := range spec.logReceivers {
			logReceivers = append(logReceivers, r.ID)
		}
		fmt.Fprintf(&sb, "    logs:\n      receivers: [%s]\n", strings.Join(logReceivers, ", "))
		sb.WriteString("      processors: [memory_limiter, zharp_logs, resourcedetection, batch]\n")
		sb.WriteString("      exporters: [zharp]\n")
	}

	// Traces and logs from applications' OpenTelemetry SDKs, forwarded as OTLP. Not through the
	// shared resourcedetection: its env detector may set service.name, and replacing the one each
	// application declares would merge every service on the box into one.
	if tracing {
		sb.WriteString(otlpPipelinesBlock)
	}

	// Host and container metrics to the OTLP ingest; databases and brokers to the zharp exporter.
	hostIDs, otherIDs := splitMetricReceivers(metricsReceivers)
	sb.WriteString(hostPipelineBlock(hostIDs))
	if collectsProcesses() {
		sb.WriteString(processPipelineBlock)
	}
	if len(otherIDs) > 0 {
		sb.WriteString(metricsPipelineBlock(otherIDs))
	}

	content := sb.String()
	content, _ = withDiskUsage(content)
	content, _ = withNetworkUsage(content)
	return content
}

// filelogOperatorsBlock returns the YAML operators block for a filelog/<name>
// receiver. Two timestamp-only regex_parser operators extract the datetime from
// the start of each log line and set the OTel record Timestamp before the record
// leaves the collector — avoiding a redundant regex pass on the backend.
// Severity is intentionally NOT extracted here; mixing it in the same operator
// caused pipeline errors when the second token was a hostname, not a level.
// The backend still has a fallback for formats the collector cannot match
// (time-only, date-only, or lines with no leading datetime at all).
func filelogOperatorsBlock(name string) string {
	var sb strings.Builder
	// Keep the tab of "\tat ..." stack-trace lines: the multi-line rules need it.
	sb.WriteString("    preserve_leading_whitespaces: true\n")
	sb.WriteString("    operators:\n")
	sb.WriteString(recombineOperator(`attributes["log.file.path"]`))
	// Quiet: lines that don't open with a date (nginx and Apache access logs start with the
	// client IP) pass on unchanged; the server reads their time and fields. Reporting each one
	// wrote two errors per request into the collector's own log.
	sb.WriteString(timestampOperatorsOnError("send_quiet"))

	// Stamp the service name on every record regardless of whether above matched.
	sb.WriteString("      - type: add\n")
	sb.WriteString("        field: resource[\"service.name\"]\n")
	fmt.Fprintf(&sb, "        value: %s\n", name)
	return sb.String()
}

// timestampOperators are the leading-timestamp parsers for files where most lines may have no
// timestamp at all (PM2 apps): such a line passes through without an error in the agent's log.
func timestampOperators() string { return timestampOperatorsOnError("send_quiet") }

func timestampOperatorsOnError(onError string) string {
	var sb strings.Builder

	// ISO 8601 T-separator: 2026-06-13T21:00:38.389230+00:00
	sb.WriteString("      - type: regex_parser\n")
	sb.WriteString("        regex: '^(?P<log_ts>\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}(?:\\.\\d+)?(?:Z|[+-]\\d{2}:?\\d{2})?)'\n")
	sb.WriteString("        timestamp:\n")
	sb.WriteString("          parse_from: attributes.log_ts\n")
	sb.WriteString("          layout_type: gotime\n")
	sb.WriteString("          layout: '2006-01-02T15:04:05.999999999Z07:00'\n")
	sb.WriteString("        on_error: send\n")
	sb.WriteString("      - type: remove\n")
	sb.WriteString("        field: attributes.log_ts\n")
	sb.WriteString("        on_error: send\n")

	// ISO 8601 space-separator: 2026-06-13 21:00:38.456
	// No timezone in regex/layout — captured value is treated as UTC.
	sb.WriteString("      - type: regex_parser\n")
	sb.WriteString("        regex: '^(?P<log_ts2>\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}(?:\\.\\d+)?)'\n")
	sb.WriteString("        timestamp:\n")
	sb.WriteString("          parse_from: attributes.log_ts2\n")
	sb.WriteString("          layout_type: gotime\n")
	sb.WriteString("          layout: '2006-01-02 15:04:05.999999999'\n")
	sb.WriteString("        on_error: send\n")
	sb.WriteString("      - type: remove\n")
	sb.WriteString("        field: attributes.log_ts2\n")
	sb.WriteString("        on_error: send\n")
	return strings.ReplaceAll(sb.String(), "        on_error: send\n", "        on_error: "+onError+"\n")
}
