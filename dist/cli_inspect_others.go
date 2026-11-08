//go:build !linux

package main

// The read-only helper is Linux's: Windows runs the agent as LocalSystem, and folder sizes are
// collected on Linux.

func ensureInspectHelper(string) string { return "" }

func removeInspectHelper() {}
