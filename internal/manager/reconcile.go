package manager

import (
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// Decision is what reconciliation concluded about one attempt or process.
type Decision string

const (
	// Adopted: the table entry's helper is alive and is the same process.
	Adopted Decision = "adopted"
	// Lost: the table entry's helper is gone, or its pid now belongs to
	// another process.
	Lost Decision = "lost"
	// Orphan: a live helper that no table entry claims.
	Orphan Decision = "orphan"
)

// Finding is one reconciliation result.
type Finding struct {
	Decision Decision
	Attempt  string
	PID      int
	Reason   string
}

// helperAttempt reports whether args is a helper started from hostBin, and
// for which attempt. The executable must be exactly hostBin and the first
// argument exactly "helper".
func helperAttempt(args, hostBin string) (string, bool) {
	rest, ok := strings.CutPrefix(args, hostBin+" helper ")
	if !ok {
		return "", false
	}
	f := strings.Fields(rest)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "--" {
			break
		}
		if f[i] == "--attempt" {
			return f[i+1], true
		}
	}
	return "", false
}

// Reconcile compares the table's live entries with the live processes.
//
// A process is a candidate helper only if it runs as uid, its executable is
// hostBin and its first argument is "helper". Nothing else is ever reported
// as an orphan, so nothing else is ever signalled.
//
// An entry is adopted only if its pid is alive, the process start time equals
// the one recorded when the helper was spawned, and the process is a helper
// for the entry's own attempt. Any mismatch means the pid was reused and the
// entry's helper is lost.
func Reconcile(entries []Entry, ps []proc.Process, uid int, hostBin string) []Finding {
	var out []Finding
	claimed := map[int]bool{}
	for _, e := range entries {
		if e.Terminal() {
			continue
		}
		p, ok := proc.Find(ps, e.PID)
		switch {
		case !ok:
			out = append(out, Finding{Lost, e.Attempt, e.PID, "no process with this pid"})
		case p.Start != e.Start:
			out = append(out, Finding{Lost, e.Attempt, e.PID, "pid reused: start time differs"})
		case p.UID != uid:
			out = append(out, Finding{Lost, e.Attempt, e.PID, "pid reused: owned by another user"})
		default:
			a, isHelper := helperAttempt(p.Args, hostBin)
			if !isHelper || a != e.Attempt {
				out = append(out, Finding{Lost, e.Attempt, e.PID, "pid reused: not this attempt's helper"})
				continue
			}
			claimed[e.PID] = true
			out = append(out, Finding{Adopted, e.Attempt, e.PID, "alive, same process"})
		}
	}
	for _, p := range ps {
		if p.UID != uid || claimed[p.PID] {
			continue
		}
		if a, ok := helperAttempt(p.Args, hostBin); ok {
			out = append(out, Finding{Orphan, a, p.PID, "live helper with no live table entry"})
		}
	}
	return out
}
