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
	"time"
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

// LockRun takes the run lock or reports the PID that holds it. A headless
// owner, one without a TUI, is marked so `__control stop` may interrupt it.
func LockRun(stateDir, runID string, headless bool) (*RunLock, error) {
	owner := strconv.Itoa(os.Getpid())
	if headless {
		owner += " headless"
	}
	return lockRun(stateDir, runID, owner)
}

// lockRun is LockRun that records owner, or no owner when it is "", so
// RunOwner reports nobody driving the run while the lock is held.
func lockRun(stateDir, runID, owner string) (*RunLock, error) {
	dir := RunDir(stateDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for range 5 {
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		time.Sleep(20 * time.Millisecond) // a RunOwner probe holds the lock only for microseconds
	}
	if err != nil {
		pid, _ := readOwner(file)
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (pid %d)", ErrRunBusy, pid)
		}
		return nil, err
	}
	_ = file.Truncate(0)
	_, _ = file.WriteAt([]byte(owner), 0)
	return &RunLock{file: file}, nil
}

// TryLockMerge takes the lock, next to the integration worktree, that lets
// one approval at a time merge and verify there, across engines and
// processes. It returns a nil lock while an approval holds it.
func TryLockMerge(integrationPath string) (*RunLock, error) {
	file, err := os.OpenFile(filepath.Join(filepath.Dir(integrationPath), "merge.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
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
	pid, _, ok := RunOwnerHeadless(stateDir, runID)
	return pid, ok
}

// RunOwnerHeadless is RunOwner that also says whether the owner is headless,
// such as a detached run, rather than a TUI.
func RunOwnerHeadless(stateDir, runID string) (pid int, headless, ok bool) {
	file, err := os.OpenFile(filepath.Join(RunDir(stateDir, runID), "owner.lock"), os.O_RDWR, 0o600)
	if err != nil {
		return 0, false, false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return 0, false, false
	}
	pid, headless = readOwner(file)
	return pid, headless, pid > 0
}

// readOwner parses "<pid>" or "<pid> headless".
func readOwner(file *os.File) (pid int, headless bool) {
	buf := make([]byte, 32)
	n, _ := file.ReadAt(buf, 0)
	id, kind, _ := strings.Cut(strings.TrimSpace(string(buf[:n])), " ")
	pid, _ = strconv.Atoi(id)
	return pid, kind == "headless"
}
