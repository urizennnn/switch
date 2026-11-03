//go:build !windows

package acpdriver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestReapOrphansKillsOnlyAbandonedAdapters(t *testing.T) {
	dir := t.TempDir()
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	record := func(owner, pid int, command ...string) {
		data, _ := json.Marshal(processRecord{Owner: owner, PID: pid, Command: command})
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadOwner := dead.Process.Pid

	orphan := start()
	record(deadOwner, orphan.Process.Pid, "sleep", "30")
	owned := start()
	record(os.Getpid(), owned.Process.Pid, "sleep", "30") // its owner is alive
	reused := start()
	record(deadOwner, reused.Process.Pid, "node", "codex-acp") // PID now runs something else

	reaped, err := ReapOrphans(dir)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapOrphans = %d, %v; want 1", reaped, err)
	}
	waitExit := func(cmd *exec.Cmd) bool {
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	if !waitExit(orphan) {
		t.Fatal("orphan was not killed")
	}
	if !processAlive(owned.Process.Pid) || !processAlive(reused.Process.Pid) {
		t.Fatal("killed a process that was not an orphan")
	}
	if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(owned.Process.Pid)+".json")); err != nil {
		t.Fatal("removed the record of a live owner")
	}
}
