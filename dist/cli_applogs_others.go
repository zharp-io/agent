//go:build !linux

package main

import (
	"errors"
	"os"
)

func ensureAgentUser() {}

var grantPM2Access = func(string) error { return errors.New("PM2 log access is only set up on Linux") }

// prepareStateDir creates the directory the collector keeps its positions in. Outside Linux the
// service runs as an administrative account, so ownership is left as it is.
func prepareStateDir() error {
	return os.MkdirAll(installStateDir(), 0o750)
}

func grantDockerAccess() {}
