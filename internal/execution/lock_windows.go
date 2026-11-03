//go:build windows

package execution

import (
	"errors"
	"path/filepath"
)

// Windows is out of scope for v1; runs are not locked there.

var ErrRunBusy = errors.New("run is driven by another Switch process")

type RunLock struct{}

func RunDir(stateDir, runID string) string {
	if len(runID) > 8 {
		runID = runID[:8]
	}
	return filepath.Join(stateDir, "runs", runID)
}

func LockRun(stateDir, runID string) (*RunLock, error) { return &RunLock{}, nil }

func (l *RunLock) Release() {}

func RunOwner(stateDir, runID string) (int, bool) { return 0, false }
