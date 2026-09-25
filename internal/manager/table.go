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

	"github.com/zeroemployeeorg/pomar/internal/capacity"
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
	// Class is the job class the attempt was admitted as; its caps are the
	// guest's. An entry from before classes counts as capacity.CI.
	Class capacity.Class `json:"class,omitzero"`
	// HostCondition names a condition of the host that compromised the
	// attempt while it was live; capacity.ReasonHostDiskFull is the first.
	// An attempt with one ends failed with it as its reason, whatever its
	// exit code (elders' ruling r9 §2).
	HostCondition string `json:"host_condition,omitempty"`
	// DiskFull is read only from tables written before HostCondition.
	DiskFull bool `json:"disk_full,omitempty"`
	// Source is the commit the attempt was pinned to at admission.
	Source *PinnedSource `json:"source,omitempty"`
	// GoProxy records that the attempt was given the module proxy.
	GoProxy bool `json:"goproxy,omitempty"`
	// Pins are what the attempt ran with, copied at admission, so the record
	// states what it ran, not what the manager runs now (DESIGN-01).
	Pins *Pins `json:"pins,omitempty"`
	// Peaks is what the attempt was measured to use.
	Peaks Peaks `json:"peaks,omitzero"`

	freeAtStart int64     // the data root's free space when first sampled
	lastSample  time.Time // the last footprint reading
}

// PinnedSource records the ref an attempt asked for and the commit it got.
type PinnedSource struct {
	Mirror string `json:"mirror"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	Git    bool   `json:"git,omitempty"`
}

// claimClass is the class the entry holds a claim as.
func (e Entry) claimClass() capacity.Class {
	if e.Class == (capacity.Class{}) {
		return capacity.CI
	}
	return e.Class
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
		if e.DiskFull && e.HostCondition == "" {
			e.HostCondition = capacity.ReasonHostDiskFull
		}
		e.DiskFull = false
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

// Pins are an attempt's inputs other than its source: the guest's pinned
// artifacts, the Pomar build, and the command's hash.
type Pins struct {
	Kernel      string `json:"kernel_sha256"`
	Init        string `json:"vminit_digest"`
	Image       string `json:"image_digest"`
	ImageArm64  string `json:"image_arm64"`
	PackageSet  string `json:"package_set,omitempty"`
	Pomar       string `json:"pomar_version"`
	HostBin     string `json:"host_bin_sha256"`
	CommandHash string `json:"command_sha256"`
}
