//go:build !windows

package execution

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLockRunWaitsOutAnOwnerProbe(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(RunDir(stateDir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Another process's RunOwner holds the lock for a moment while it checks.
	probe, err := os.OpenFile(filepath.Join(RunDir(stateDir, "run"), "owner.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(5*time.Millisecond, func() { probe.Close() }) // closing drops the lock

	lock, err := LockRun(stateDir, "run", false)
	if err != nil {
		t.Fatalf("LockRun during a probe: %v", err)
	}
	lock.Release()
}

func TestRunOwnerSaysWhetherTheOwnerIsHeadless(t *testing.T) {
	stateDir := t.TempDir()
	for _, headless := range []bool{false, true} {
		lock, err := LockRun(stateDir, "run", headless)
		if err != nil {
			t.Fatal(err)
		}
		pid, gotHeadless, ok := RunOwnerHeadless(stateDir, "run")
		if !ok || pid != os.Getpid() || gotHeadless != headless {
			t.Fatalf("RunOwnerHeadless = %d, %v, %v; want %d, %v, true", pid, gotHeadless, ok, os.Getpid(), headless)
		}
		if pid, ok := RunOwner(stateDir, "run"); !ok || pid != os.Getpid() {
			t.Fatalf("RunOwner = %d, %v", pid, ok)
		}
		lock.Release()
	}
	if _, _, ok := RunOwnerHeadless(stateDir, "run"); ok {
		t.Fatal("owner reported after release")
	}
}
