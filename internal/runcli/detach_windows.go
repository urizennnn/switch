//go:build windows

package runcli

import (
	"errors"
	"os/exec"
)

// Windows is out of scope for v1.
func startDetached(cmd *exec.Cmd) error {
	return errors.New("detached runs are not supported on Windows")
}

func interrupt(pid int) error { return errors.New("detached runs are not supported on Windows") }
