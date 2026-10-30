// Package dockerstatereceiver is the docker_state receiver: every container's state, exit code,
// OOM kill, times, ports and restart count, through the Docker Engine API.
//
// It lives in dockerlogsreceiver, whose API client it shares; this package only gives the
// collector builder a path of its own to import the factory from.
package dockerstatereceiver

import (
	"go.opentelemetry.io/collector/receiver"

	"github.com/zharp-io/agent/dockerlogsreceiver"
)

// NewFactory creates the docker_state receiver factory.
func NewFactory() receiver.Factory {
	return dockerlogsreceiver.NewStateFactory()
}
