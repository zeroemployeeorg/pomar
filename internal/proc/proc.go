// Package proc lists live processes. The manager's reconciliation uses it to
// tell a helper that is still running from a pid that has been reused.
package proc

import (
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Process is one live process.
type Process struct {
	PID int
	UID int
	// Start is the process start time as ps prints it (lstart). Together
	// with the pid it identifies a process: a reused pid has a new start.
	Start string
	// CPU is the accumulated CPU time. For a guest it is read from its VM
	// service, never from the helper, which only waits.
	CPU time.Duration
	// Args is the full command line.
	Args string
}

// Lister returns the live processes. Tests inject their own.
type Lister interface {
	List() ([]Process, error)
}

// PS lists processes with ps(1).
type PS struct{}

// List runs `ps -axww -o pid=,uid=,lstart=,time=,command=`.
func (PS) List() ([]Process, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,uid=,lstart=,time=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("proc: ps: %w", err)
	}
	return Parse(string(out))
}

// Parse reads ps output in the format List requests. lstart is five fields
// ("Wed Sep 23 21:08:10 2026"); time is [[dd-]hh:]mm:ss.cc.
func Parse(out string) ([]Process, error) {
	var ps []Process
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 9 {
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
		cpu, err := ParseCPU(f[7])
		if err != nil {
			return nil, fmt.Errorf("proc: line %d: %w", i+1, err)
		}
		ps = append(ps, Process{
			PID:   pid,
			UID:   uid,
			Start: strings.Join(f[2:7], " "),
			CPU:   cpu,
			Args:  strings.Join(f[8:], " "),
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

// ParseCPU reads ps's time column: [[dd-]hh:]mm:ss.cc, where the leading
// field may exceed its usual range ("123:04.50" is 123 minutes).
func ParseCPU(s string) (time.Duration, error) {
	bad := fmt.Errorf("proc: cpu time %q", s)
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil || n < 0 {
			return 0, bad
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || sec < 0 {
		return 0, bad
	}
	total := time.Duration(days) * 24 * time.Hour
	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil || n < 0 {
			return 0, bad
		}
		total += time.Duration(n) * unit
		unit *= 60
	}
	return total + time.Duration(math.Round(sec*1000))*time.Millisecond, nil
}
