package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Paths inside the data root.
const (
	managerDir  = "manager"  // ledgered cache: table, lock, socket, events
	attemptsDir = "attempts" // venue structure; one ledgered record per attempt
	storeDir    = "store"
	tmpDir      = "tmp"
)

// Guest holds the pinned artifacts every helper boots from.
type Guest struct {
	Kernel      string // absolute path
	InitRef     string
	InitDigest  string
	ImageRef    string
	ImageDigest string
}

// Config is what a manager needs.
type Config struct {
	Venue   *venue.Venue
	HostBin string // absolute path of the signed pomar-host
	Guest   Guest
	Procs   proc.Lister
	UID     int
	// ReapWait bounds how long an orphan gets between SIGTERM and SIGKILL.
	ReapWait time.Duration
	// Poll is the liveness check interval.
	Poll time.Duration
	Log  io.Writer
}

// Manager supervises helpers.
type Manager struct {
	cfg    Config
	dir    string
	lock   *os.File
	mu     sync.Mutex
	t      *table
	report []Finding
	events *os.File
	done   chan struct{}
}

// Open takes the manager lock, loads the table and reconciles it against the
// live processes. Only one manager may hold a data root at a time.
func Open(cfg Config) (*Manager, error) {
	v := cfg.Venue
	if cfg.ReapWait == 0 {
		cfg.ReapWait = 20 * time.Second
	}
	if cfg.Poll == 0 {
		cfg.Poll = time.Second
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if err := v.Init(); err != nil {
		return nil, err
	}
	if err := v.EnsureCache(venue.KindVolume, "manager", managerDir); err != nil {
		return nil, err
	}
	if err := v.EnsureCache(venue.KindVolume, "tmp", tmpDir); err != nil {
		return nil, err
	}
	dir := filepath.Join(v.Root(), managerDir)
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("manager: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("manager: another manager holds this data root: %w", err)
	}
	t, err := loadTable(tablePath(dir))
	if err != nil {
		lock.Close()
		return nil, err
	}
	ev, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("manager: %w", err)
	}
	m := &Manager{cfg: cfg, dir: dir, lock: lock, t: t, events: ev, done: make(chan struct{})}
	m.event("manager-start", "", 0, "")
	if err := m.reconcile(); err != nil {
		m.Close()
		return nil, err
	}
	go m.watch()
	return m, nil
}

// Close stops watching and releases the lock. Helpers keep running.
func (m *Manager) Close() error {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	m.events.Close()
	return m.lock.Close()
}

// Report returns the findings of the start-up reconciliation.
func (m *Manager) Report() []Finding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Finding(nil), m.report...)
}

func (m *Manager) event(kind, attempt string, pid int, detail string) {
	b, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC(), "event": kind, "attempt": attempt, "pid": pid, "detail": detail,
	})
	m.events.Write(append(b, '\n'))
	m.events.Sync()
	fmt.Fprintf(m.cfg.Log, "%s attempt=%s pid=%d %s\n", kind, attempt, pid, detail)
}

// reconcile applies Reconcile's findings. It runs once, at start.
func (m *Manager) reconcile() error {
	ps, err := m.cfg.Procs.List()
	if err != nil {
		return err
	}
	m.mu.Lock()
	entries := m.t.list()
	m.mu.Unlock()
	findings := Reconcile(entries, ps, m.cfg.UID, m.cfg.HostBin)
	for _, f := range findings {
		m.event("reconcile-"+string(f.Decision), f.Attempt, f.PID, f.Reason)
		switch f.Decision {
		case Adopted:
			// Nothing to do: the watcher picks it up. The helper was never
			// signalled and its guest is untouched.
		case Lost:
			m.finish(f.Attempt, StateLost, "reconcile: "+f.Reason, nil)
		case Orphan:
			m.reap(f)
		}
	}
	m.mu.Lock()
	m.report = findings
	err = m.t.save()
	m.mu.Unlock()
	return err
}

// reap stops an orphaned helper: SIGTERM, a bounded wait for it to tear its
// guest down, then SIGKILL. Its VM object, if still ledgered, is torn down.
func (m *Manager) reap(f Finding) {
	sig := func(s syscall.Signal) { syscall.Kill(f.PID, s) }
	sig(syscall.SIGTERM)
	m.event("reap-sigterm", f.Attempt, f.PID, "")
	deadline := time.Now().Add(m.cfg.ReapWait)
	for time.Now().Before(deadline) {
		if !m.sameHelperAlive(f.PID, f.Attempt) {
			m.event("reap-exited", f.Attempt, f.PID, "after SIGTERM")
			m.teardownVM(f.Attempt)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !m.sameHelperAlive(f.PID, f.Attempt) {
		m.event("reap-exited", f.Attempt, f.PID, "at the deadline")
		m.teardownVM(f.Attempt)
		return
	}
	sig(syscall.SIGKILL)
	m.event("reap-sigkill", f.Attempt, f.PID, fmt.Sprintf("no exit within %s", m.cfg.ReapWait))
	m.teardownVM(f.Attempt)
}

// sameHelperAlive reports whether pid is still a helper for attempt.
func (m *Manager) sameHelperAlive(pid int, attempt string) bool {
	ps, err := m.cfg.Procs.List()
	if err != nil {
		return true // unknown is treated as alive: never declare an end we did not see
	}
	p, ok := proc.Find(ps, pid)
	if !ok || p.UID != m.cfg.UID {
		return false
	}
	a, isHelper := helperAttempt(p.Args, m.cfg.HostBin)
	return isHelper && a == attempt
}

func (m *Manager) teardownVM(attempt string) {
	v := m.cfg.Venue
	if v.IsOpen(venue.KindVM, attempt) {
		if err := v.Teardown(venue.KindVM, attempt); err != nil {
			m.event("teardown-error", attempt, 0, err.Error())
		}
	}
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ErrExists is returned when an attempt id is already in the table.
var ErrExists = errors.New("manager: attempt exists")

// Start ledgers an attempt's objects and spawns its helper in a new session,
// so that the manager's own death does not signal it.
func (m *Manager) Start(id string, command []string) (Entry, error) {
	v := m.cfg.Venue
	if !validID.MatchString(id) {
		return Entry{}, fmt.Errorf("manager: invalid attempt id %q", id)
	}
	if len(command) == 0 {
		return Entry{}, errors.New("manager: empty command")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.t.entries[id]; ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrExists, id)
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		return Entry{}, err
	}
	recRel := filepath.Join(attemptsDir, id)
	recID := "attempt-" + id
	if err := v.Intent(venue.KindVolume, venue.ClassAttempt, recID, recRel, "attempt record"); err != nil {
		return Entry{}, err
	}
	rec := filepath.Join(v.Root(), recRel)
	if err := os.MkdirAll(rec, 0o700); err != nil {
		v.Failed(venue.KindVolume, recID, err.Error())
		return Entry{}, err
	}
	if err := v.Created(venue.KindVolume, recID); err != nil {
		return Entry{}, err
	}
	if err := v.Intent(venue.KindVM, venue.ClassAttempt, id, filepath.Join(storeDir, "containers", id), "helper-owned guest"); err != nil {
		return Entry{}, err
	}

	g := m.cfg.Guest
	args := []string{"helper", "--attempt", id, "--state-dir", rec,
		"--store", filepath.Join(v.Root(), storeDir), "--kernel", g.Kernel,
		"--init", g.InitRef, "--init-digest", g.InitDigest,
		"--image", g.ImageRef, "--image-digest", g.ImageDigest, "--"}
	args = append(args, command...)
	logf, err := os.OpenFile(filepath.Join(rec, "helper.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		v.Failed(venue.KindVM, id, err.Error())
		return Entry{}, err
	}
	cmd := exec.Command(m.cfg.HostBin, args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(v.Root(), tmpDir)+string(filepath.Separator))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		v.Failed(venue.KindVM, id, err.Error())
		v.Teardown(venue.KindVM, id)
		return Entry{}, err
	}
	logf.Close()
	pid := cmd.Process.Pid
	// Reap our own child when it exits, so it never lingers as a zombie
	// that looks alive. A helper adopted after a restart is not our child;
	// launchd reaps it.
	go cmd.Wait()

	start, err := m.startTime(pid)
	if err != nil {
		// Without a start time the entry could never be adopted safely. Stop
		// the helper we just made.
		syscall.Kill(pid, syscall.SIGTERM)
		m.event("start-error", id, pid, err.Error())
		return Entry{}, err
	}
	e := &Entry{Attempt: id, Command: command, PID: pid, Start: start, State: StateStarting, Created: time.Now().UTC()}
	m.t.entries[id] = e
	if err := m.t.save(); err != nil {
		return Entry{}, err
	}
	if err := v.Created(venue.KindVM, id); err != nil {
		return Entry{}, err
	}
	m.event("start", id, pid, start)
	return *e, nil
}

func (m *Manager) startTime(pid int) (string, error) {
	for i := 0; i < 10; i++ {
		ps, err := m.cfg.Procs.List()
		if err == nil {
			if p, ok := proc.Find(ps, pid); ok {
				return p.Start, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("manager: pid %d not visible to ps", pid)
}

// Stop asks a live attempt's helper to stop. The attempt stays "stopping"
// until the helper has actually exited.
func (m *Manager) Stop(id string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.t.entries[id]
	if !ok {
		return Entry{}, fmt.Errorf("manager: no attempt %s", id)
	}
	if e.Terminal() {
		return *e, nil
	}
	// Re-check identity right before signalling: the helper may have ended
	// since the last poll and its pid been reused.
	if !m.sameHelperAlive(e.PID, id) {
		return *e, fmt.Errorf("manager: helper for %s is not running; its end will be recorded", id)
	}
	if err := syscall.Kill(e.PID, syscall.SIGTERM); err != nil {
		return *e, fmt.Errorf("manager: signalling helper: %w", err)
	}
	e.State = StateStopping
	m.event("stop-requested", id, e.PID, "")
	return *e, m.t.save()
}

// Remove deletes a terminal attempt's record from the table and the data root.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.t.entries[id]
	if !ok {
		return fmt.Errorf("manager: no attempt %s", id)
	}
	if !e.Terminal() {
		return fmt.Errorf("manager: attempt %s is %s; stop it first", id, e.State)
	}
	v := m.cfg.Venue
	if v.IsOpen(venue.KindVolume, "attempt-"+id) {
		if err := v.Teardown(venue.KindVolume, "attempt-"+id); err != nil {
			return err
		}
	}
	delete(m.t.entries, id)
	m.event("removed", id, 0, "")
	return m.t.save()
}

// List returns every attempt. A live helper is never reported as ended.
func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.list()
}

// watch polls live entries and finishes those whose helper has gone.
func (m *Manager) watch() {
	tick := time.NewTicker(m.cfg.Poll)
	defer tick.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-tick.C:
		}
		ps, err := m.cfg.Procs.List()
		if err != nil {
			continue // unknown: change nothing
		}
		m.mu.Lock()
		var live []Entry
		for _, e := range m.t.entries {
			if !e.Terminal() {
				live = append(live, *e)
			}
		}
		m.mu.Unlock()
		for _, e := range live {
			p, ok := proc.Find(ps, e.PID)
			a, isHelper := helperAttempt(p.Args, m.cfg.HostBin)
			if ok && p.Start == e.Start && isHelper && a == e.Attempt {
				m.observe(e.Attempt)
				continue
			}
			st, reason, code := m.terminalFromStatus(e.Attempt)
			m.finish(e.Attempt, st, reason, code)
		}
	}
}

// observe moves a live attempt from starting to running once its helper says so.
func (m *Manager) observe(id string) {
	phase, _ := m.readStatus(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.t.entries[id]
	if e != nil && e.State == StateStarting && phase["phase"] == "running" {
		e.State = StateRunning
		m.t.save()
		m.event("running", id, e.PID, "")
	}
}

func (m *Manager) readStatus(id string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, "status.json"))
	if err != nil {
		return nil, err
	}
	var s map[string]string
	return s, json.Unmarshal(b, &s)
}

// terminalFromStatus reads the ended helper's last status.
func (m *Manager) terminalFromStatus(id string) (State, string, *int) {
	s, err := m.readStatus(id)
	if err != nil {
		return StateLost, "helper gone; no status", nil
	}
	switch s["phase"] {
	case "exited":
		var code int
		fmt.Sscan(s["exit_code"], &code)
		return StateExited, "", &code
	case "stopped":
		return StateStopped, "", nil
	case "failed":
		return StateFailed, s["error"], nil
	}
	return StateLost, "helper gone in phase " + s["phase"], nil
}

// finish records an attempt's end and tears its guest's object down.
func (m *Manager) finish(id string, st State, reason string, code *int) {
	m.mu.Lock()
	e := m.t.entries[id]
	if e == nil || e.Terminal() {
		m.mu.Unlock()
		return
	}
	e.State, e.Reason, e.ExitCode, e.Ended = st, reason, code, time.Now().UTC()
	m.t.save()
	m.mu.Unlock()
	m.event("ended-"+string(st), id, e.PID, reason)
	m.teardownVM(id)
}
