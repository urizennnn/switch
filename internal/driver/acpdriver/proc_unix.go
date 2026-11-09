//go:build !windows

package acpdriver

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the adapter in its own process group so that a
// cancelled probe also reaps node/npx children.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func killGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// terminateGroup asks the adapter and its children to exit.
func terminateGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}
