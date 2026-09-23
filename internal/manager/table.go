// Package manager supervises one VM-owning helper process per attempt, keeps
// the attempt table, and reconciles that table against live processes every
// time it starts. It never touches a VM itself: helpers own VMs, so the
// manager can die and restart without killing any guest.
package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State is an attempt's state as the manager knows it.
type State string

const (
	StateStarting State = "starting" // helper spawned, guest not yet running
	StateRunning  State = "running"  // helper reports the guest running
	StateStopping State = "stopping" // stop requested; helper still alive
	StateExited   State = "exited"   // command ended on its own
	StateStopped  State = "stopped"  // ended by a stop request
	StateFailed   State = "failed"   // helper reported a failure
	StateLost     State = "lost"     // helper gone without a terminal status
)

// Entry is one attempt.
type Entry struct {
	Attempt string   `json:"attempt"`
	Command []string `json:"command"`
	PID     int      `json:"pid"`
	// Start is the helper's process start time, read from ps right after
	// spawning. Reconciliation requires it to match.
	Start    string    `json:"start"`
	State    State     `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Created  time.Time `json:"created"`
	Ended    time.Time `json:"ended,omitzero"`
}

// Terminal reports whether the attempt has ended.
func (e Entry) Terminal() bool {
	switch e.State {
	case StateExited, StateStopped, StateFailed, StateLost:
		return true
	}
	return false
}

type table struct {
	path    string
	entries map[string]*Entry
}

func loadTable(path string) (*table, error) {
	t := &table{path: path, entries: map[string]*Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manager: %w", err)
	}
	var list []*Entry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("manager: table: %w", err)
	}
	for _, e := range list {
		t.entries[e.Attempt] = e
	}
	return t, nil
}

// save writes the table atomically: a temp file in the same directory, synced,
// then renamed over the old one.
func (t *table) save() error {
	b, err := json.MarshalIndent(t.list(), "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("manager: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("manager: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	return nil
}

func (t *table) list() []Entry {
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func tablePath(managerDir string) string { return filepath.Join(managerDir, "table.json") }
