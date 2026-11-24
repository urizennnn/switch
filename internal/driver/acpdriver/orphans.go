package acpdriver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Adapter processes run in their own process groups, so they outlive a
// Switch process that crashes. TrackProcesses records each one; ReapOrphans
// kills those whose owner is gone.

var (
	trackMu  sync.Mutex
	trackDir string
)

type processRecord struct {
	Owner   int      `json:"owner"`
	PID     int      `json:"pid"`
	Command []string `json:"command"`
}

// TrackProcesses records adapter processes started from now on under dir.
func TrackProcesses(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	trackMu.Lock()
	trackDir = dir
	trackMu.Unlock()
	return nil
}

// track records a started adapter and returns a func that forgets it.
func track(pid int, command []string) func() {
	trackMu.Lock()
	dir := trackDir
	trackMu.Unlock()
	if dir == "" {
		return func() {}
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	data, _ := json.Marshal(processRecord{Owner: os.Getpid(), PID: pid, Command: command})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return func() {}
	}
	return func() { _ = os.Remove(path) }
}

// ReapOrphans kills adapter process groups recorded by Switch processes that
// are no longer running, and returns how many it stopped. A record is only
// acted on when its PID still runs the recorded command, so a reused PID is
// never killed.
func ReapOrphans(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record processRecord
		if json.Unmarshal(data, &record) != nil || record.PID <= 0 || len(record.Command) == 0 {
			_ = os.Remove(path)
			continue
		}
		if processAlive(record.Owner) {
			continue // another Switch is still using it
		}
		if processAlive(record.PID) && runsCommand(record.PID, record.Command) {
			if killGroup(record.PID) == nil {
				reaped++
			}
		}
		_ = os.Remove(path)
	}
	return reaped, nil
}

// runsCommand reports whether pid's command line still starts with the
// recorded program.
func runsCommand(pid int, command []string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	program := filepath.Base(command[0])
	if program == npxBinary {
		program = "npm exec" // npm retitles npx as "npm exec <package>"
	}
	return line != "" && strings.Contains(line, program) &&
		(len(command) < 2 || strings.Contains(line, command[len(command)-1]))
}
