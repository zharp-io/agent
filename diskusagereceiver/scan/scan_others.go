//go:build !linux

package scan

import (
	"errors"
	"io/fs"
)

// Folder sizes are collected on Linux; elsewhere a scan needs its drives named in Options.Mounts.

func drives() ([]string, error) {
	return nil, errors.New("pass the folders to scan (drive detection is Linux only)")
}

func deviceOf(string) (uint64, bool) { return 0, false }

func diskBytes(info fs.FileInfo) int64 { return info.Size() }

func ownerOf(fs.FileInfo) string { return "" }

func lowerPriority() {}

func canReadEverything() bool { return false }
