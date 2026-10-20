package dockerlogsreceiver

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zharp-io/agent/logrules"
)

// Config is the docker_logs receiver's configuration.
type Config struct {
	// Endpoint is the Docker Engine API socket: unix:///var/run/docker.sock, or on Windows
	// npipe:////./pipe/docker_engine.
	Endpoint string `mapstructure:"endpoint"`

	// StateFile remembers, per container, the timestamp of the last line delivered, so a restart
	// of the collector resumes where it stopped instead of replaying or skipping lines. Empty
	// keeps the position in memory only (a restart then starts again from "now").
	StateFile string `mapstructure:"state_file"`

	// Timeout bounds the short API calls (list, inspect). Log and event streams are long-lived
	// and only end when the container or the daemon goes away.
	Timeout time.Duration `mapstructure:"timeout"`

	// ExcludeLabel is the label that opts a container out: a container labelled
	// <ExcludeLabel>=false is not read. Defaults to zharp.logs.
	ExcludeLabel string `mapstructure:"exclude_label"`

	// Multiline is how lines are joined into records when a container has no
	// zharp.logs.multiline label: "auto" (the default) joins stack traces, "off" keeps one
	// record per line, anything else is a regex matching the first line of each record.
	Multiline string `mapstructure:"multiline"`
}

func (c *Config) Validate() error {
	e := strings.TrimSpace(c.Endpoint)
	if e == "" {
		return errors.New("docker_logs: endpoint is required")
	}
	if !strings.HasPrefix(e, "unix://") && !strings.HasPrefix(e, "npipe://") {
		return errors.New("docker_logs: endpoint must be unix:///path/to/docker.sock or npipe:////./pipe/docker_engine")
	}
	if c.Timeout < 0 {
		return errors.New("docker_logs: timeout must not be negative")
	}
	if _, err := logrules.ParseMultiline(c.Multiline); err != nil {
		return fmt.Errorf("docker_logs: %w", err)
	}
	return nil
}
