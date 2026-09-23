// Package proc lists live processes. The manager's reconciliation uses it to
// tell a helper that is still running from a pid that has been reused.
package proc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Process is one live process.
type Process struct {
	PID int
	UID int
	// Start is the process start time as ps prints it (lstart). Together
	// with the pid it identifies a process: a reused pid has a new start.
	Start string
	// Args is the full command line.
	Args string
}

// Lister returns the live processes. Tests inject their own.
type Lister interface {
	List() ([]Process, error)
}

// PS lists processes with ps(1).
type PS struct{}

// List runs `ps -axww -o pid=,uid=,lstart=,command=`.
func (PS) List() ([]Process, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,uid=,lstart=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("proc: ps: %w", err)
	}
	return Parse(string(out))
}

// Parse reads ps output in the format List requests. lstart is five fields
// ("Wed Sep 23 21:08:10 2026").
func Parse(out string) ([]Process, error) {
	var ps []Process
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 8 {
			return nil, fmt.Errorf("proc: line %d: too few fields: %q", i+1, line)
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("proc: line %d: pid: %w", i+1, err)
		}
		uid, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, fmt.Errorf("proc: line %d: uid: %w", i+1, err)
		}
		ps = append(ps, Process{
			PID:   pid,
			UID:   uid,
			Start: strings.Join(f[2:7], " "),
			Args:  strings.Join(f[7:], " "),
		})
	}
	return ps, nil
}

// Find returns the process with pid, if it is live.
func Find(ps []Process, pid int) (Process, bool) {
	for _, p := range ps {
		if p.PID == pid {
			return p, true
		}
	}
	return Process{}, false
}
