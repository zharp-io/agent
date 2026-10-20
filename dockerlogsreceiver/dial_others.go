//go:build !windows

package dockerlogsreceiver

import (
	"context"
	"errors"
	"net"
)

func dialPipe(context.Context, string) (net.Conn, error) {
	return nil, errors.New("named pipes are only available on Windows")
}
