package main

import (
	"os"
	"strings"
)

// agentVersionAttribute is the resource attribute naming the agent's version on what it sends.
const agentVersionAttribute = "zharp.agent.version"

// setAgentVersionResource adds zharp.agent.version=<version> to OTEL_RESOURCE_ATTRIBUTES for
// this process, where the resourcedetection processor's env detector reads it. Set here rather
// than in the config so it is always the running binary's version, through every update.
// An existing value is kept, and one the person set for this attribute wins.
func setAgentVersionResource(version string) {
	current := strings.TrimSpace(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))
	_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", withAgentVersion(current, version))
}

func withAgentVersion(current, version string) string {
	for _, pair := range strings.Split(current, ",") {
		if key, _, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(key) == agentVersionAttribute {
			return current
		}
	}
	attr := agentVersionAttribute + "=" + version
	if current == "" {
		return attr
	}
	return current + "," + attr
}
