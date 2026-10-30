package main

import (
	"regexp"
	"strings"
)

// Host and container metrics moved from the zharp exporter (JSON to /agent/metrics) to Zharp's
// OTLP ingest, for Servers & Containers. withHostMetricsPipeline brings a config written before
// that onto the new layout, as `update` does for every install:
//
//   - the otlp_http/zharp exporter (also with --no-otlp, which only concerns the receiver);
//   - the resourcedetection/host processor;
//   - the hostmetrics receiver as install writes it now (utilization, filesystems, uptime);
//   - docker_state beside docker_stats;
//   - hostmetrics, docker_stats, docker_state and kubeletstats moved out of the metrics pipeline
//     into metrics/host, which exports through otlp_http/zharp (and zharp, for its heartbeat).
//     The metrics pipeline keeps the databases, or goes when nothing is left in it.
//
// It changes nothing in a config that already has all of it.
func withHostMetricsPipeline(content string) (string, []string) {
	var notes []string
	if !strings.Contains(content, "\nservice:") || !strings.Contains(content, "\nprocessors:\n") {
		return content, nil
	}

	if !strings.Contains(content, "\n  "+otlpExporterID+":") {
		api, apiKey := zharpExporterSettings(content)
		if apiKey == "" {
			return content, nil // not a config install wrote; leave it to its owner
		}
		content = strings.Replace(content, "\nservice:", otlpExporterBlock(api, apiKey)+"\nservice:", 1)
	}
	if !strings.Contains(content, "\n  "+hostProcessorID+":") {
		content = strings.Replace(content, "\nprocessors:\n", "\nprocessors:\n"+hostProcessorBlock, 1)
	}

	have := configReceiverIDs(content)
	if have["hostmetrics"] && !strings.Contains(content, hostmetricsBlock) {
		content = replaceReceiverBlock(content, "hostmetrics", hostmetricsBlock)
		notes = append(notes, "Host metrics: now CPU and memory use, filesystems per mount and uptime, every 30s")
	}
	if have["docker_stats"] && !have["docker_state"] {
		endpoint := receiverEndpoint(content, "docker_stats")
		block := "  docker_state:\n    endpoint: " + endpoint + "\n    collection_interval: 30s\n"
		content = strings.Replace(content, "\nprocessors:", "\n"+block+"\nprocessors:", 1)
		if hasPipeline(content, hostPipelineID) {
			content = addToPipeline(content, hostPipelineID, []string{"docker_state"})
		} else {
			content = addToPipeline(content, "metrics", []string{"docker_state"})
		}
		notes = append(notes, "Docker: container state, exit codes and restarts (docker_state)")
	}

	if hasPipeline(content, hostPipelineID) {
		return content, notes
	}
	re := pipelineListRe("metrics")
	m := re.FindStringSubmatch(content)
	if m == nil {
		return content, notes
	}
	host, other := splitMetricReceivers(splitList(m[2]))
	if len(host) == 0 {
		return content, notes
	}
	if len(other) > 0 {
		content = re.ReplaceAllString(content, "${1}"+strings.Join(other, ", ")+"]")
	} else {
		content = removePipeline(content, "metrics")
	}
	content = insertPipeline(content, hostPipelineBlock(host))
	notes = append(notes, "Host and container metrics now go to Zharp as OpenTelemetry (pipeline "+hostPipelineID+")")
	return content, notes
}

// configReceiverIDs are the ids of a config's receivers.
func configReceiverIDs(content string) map[string]bool {
	ids := map[string]bool{}
	if have, err := configReceivers(content); err == nil {
		for _, r := range have {
			ids[r.ID] = true
		}
	}
	return ids
}

// replaceReceiverBlock swaps a receiver's block (its header line and everything indented under
// it) for block, in place.
func replaceReceiverBlock(content, id, block string) string {
	header := "\n  " + id + ":\n"
	start := strings.Index(content, header)
	if start < 0 {
		return content
	}
	removed := removeReceiverBlock(content, id)
	return removed[:start+1] + block + removed[start+1:]
}

var endpointLineRe = regexp.MustCompile(`(?m)^    endpoint:\s*(\S+)\s*$`)

// receiverEndpoint is the endpoint a receiver's block names, or the platform's Docker socket.
func receiverEndpoint(content, id string) string {
	if start := strings.Index(content, "\n  "+id+":\n"); start >= 0 {
		rest := content[start+len(id)+5:]
		if end := regexp.MustCompile(`\n  \S`).FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		if m := endpointLineRe.FindStringSubmatch(rest); m != nil {
			return strings.Trim(m[1], `"'`)
		}
	}
	if strings.Contains(dockerReceiverBlocks(), "npipe") {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

// removePipeline drops a pipeline: its name line under service.pipelines and what is indented
// under it.
func removePipeline(content, name string) string {
	header := "\n    " + name + ":\n"
	start := strings.Index(content, header)
	if start < 0 {
		return content
	}
	end := start + len(header)
	for end < len(content) {
		nl := strings.IndexByte(content[end:], '\n')
		line := content[end:]
		if nl >= 0 {
			line = content[end : end+nl]
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "      ") {
			break
		}
		if nl < 0 {
			end = len(content)
			break
		}
		end += nl + 1
	}
	return content[:start+1] + content[end:]
}
