// Package dockerlogsreceiver reads container logs through the Docker Engine API.
//
// The collector runs as an unprivileged user that cannot read /var/lib/docker/containers, but it
// is in the docker group for docker_stats, so the API socket is the one way in that needs no
// extra rights. The receiver follows every running container's stdout and stderr, picks up
// containers as they start, and remembers how far it got so a restart neither replays nor drops
// lines.
package dockerlogsreceiver

import (
	"context"
	"runtime"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const typeStr = "docker_logs"

// NewFactory creates the docker_logs receiver factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithLogs(createLogsReceiver, component.StabilityLevelAlpha),
	)
}

func defaultEndpoint() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func createDefaultConfig() component.Config {
	return &Config{
		Endpoint:     defaultEndpoint(),
		Timeout:      10 * time.Second,
		ExcludeLabel: "zharp.logs",
		Multiline:    "auto",
	}
}

func createLogsReceiver(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
	c := cfg.(*Config)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return newReceiver(c, set.Logger, next)
}
