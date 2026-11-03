//go:build !windows

package runcli

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts cmd in its own session so it outlives the terminal.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func interrupt(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGINT)
}
