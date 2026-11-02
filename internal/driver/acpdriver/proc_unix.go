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
