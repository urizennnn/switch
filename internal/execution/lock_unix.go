//go:build !windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrRunBusy means another process is driving the run.
var ErrRunBusy = errors.New("run is driven by another Switch process")

// RunLock is an exclusive flock held by the one process driving a run. The
// kernel drops it if that process dies, so a crash never leaves it stale.
type RunLock struct {
	file *os.File
}

// RunDir is <state>/runs/<id8>, shared by the lock, control socket, and logs.
func RunDir(stateDir, runID string) string {
	short := runID
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join(stateDir, "runs", short)
}

// LockRun takes the run lock or reports the PID that holds it.
func LockRun(stateDir, runID string) (*RunLock, error) {
	dir := RunDir(stateDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(file)
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (pid %d)", ErrRunBusy, pid)
		}
		return nil, err
	}
	_ = file.Truncate(0)
	_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return &RunLock{file: file}, nil
}

func (l *RunLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Truncate(0)
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

// RunOwner returns the PID driving the run, or false when nobody is.
func RunOwner(stateDir, runID string) (int, bool) {
	file, err := os.OpenFile(filepath.Join(RunDir(stateDir, runID), "owner.lock"), os.O_RDWR, 0o600)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return 0, false
	}
	pid := readPID(file)
	return pid, pid > 0
}

func readPID(file *os.File) int {
	buf := make([]byte, 32)
	n, _ := file.ReadAt(buf, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	return pid
}
