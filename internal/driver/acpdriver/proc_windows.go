//go:build windows

package acpdriver

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}

// Orphan reaping is Unix-only; Windows is out of scope for v1.
func processAlive(pid int) bool { return false }

func killGroup(pid int) error { return nil }

func terminateGroup(pid int) error { return nil }
