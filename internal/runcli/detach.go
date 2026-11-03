package runcli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/execution"
)

// Detach starts a background `switch __control run --wait` for the run and
// returns once it holds the run lock. The run keeps going after the TUI or
// terminal exits; approve its tasks from the TUI or with `__control approve`.
func Detach(project, stateDir, runID, permission string) (pid int, logPath string, err error) {
	if pid, ok := execution.RunOwner(stateDir, runID); ok {
		return 0, "", fmt.Errorf("%w (pid %d)", execution.ErrRunBusy, pid)
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	dir := execution.RunDir(stateDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", err
	}
	logPath = filepath.Join(dir, "run.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "__control", "run", "--cwd", project, "--run", runID, "--permission", permission, "--wait")
	cmd.Dir = project
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := startDetached(cmd); err != nil {
		return 0, "", err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(15 * time.Second)
	for {
		if owner, ok := execution.RunOwner(stateDir, runID); ok && owner == cmd.Process.Pid {
			return owner, logPath, nil
		}
		select {
		case <-exited:
			return 0, logPath, fmt.Errorf("detached run exited at startup: %s", tailFile(logPath, 5))
		case <-deadline:
			return cmd.Process.Pid, logPath, fmt.Errorf("detached run (pid %d) did not take the run lock; see %s", cmd.Process.Pid, logPath)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop interrupts the process driving the run and waits for it to let go.
// Its running tasks become interrupted, the same as /stop in the TUI.
func Stop(stateDir, runID string) error {
	pid, ok := execution.RunOwner(stateDir, runID)
	if !ok {
		return fmt.Errorf("run %s is not running", shortID(runID))
	}
	if pid == os.Getpid() {
		return fmt.Errorf("this process drives the run; use /stop")
	}
	if err := interrupt(pid); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := execution.RunOwner(stateDir, runID); !ok {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not stop within 30s", pid)
}

func tailFile(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	all := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
