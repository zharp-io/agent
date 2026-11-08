//go:build !linux

package conns

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("measuring connections is only supported on Linux")

// Supported reports whether this platform can measure connections.
func Supported() bool { return false }

// Dump is Linux only.
func Dump() ([]Socket, error) { return nil, errUnsupported }

// Watch is Linux only.
func Watch(context.Context, func(Socket)) error { return errUnsupported }

// Processes is Linux only.
func Processes(string) Owners { return Owners{} }

// HostNetwork is Linux only.
func HostNetwork(string) (bool, bool) { return false, false }

// UserName is Linux only.
func UserName(uid uint32) string { return "" }
