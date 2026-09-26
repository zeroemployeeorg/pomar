package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/cache"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/goproxy"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/result"
	"github.com/zeroemployeeorg/pomar/internal/sign"
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
	Kernel       string // absolute path
	KernelSHA256 string // the pinned kernel's hash, recorded in each attempt
	InitRef      string
	InitDigest   string
	ImageRef     string
	ImageDigest  string
	// ImageArm64 and PackageSet name the base the guest boots from.
	ImageArm64 string
	PackageSet string
}

// Config is what a manager needs.
type Config struct {
	Venue   *venue.Venue
	HostBin string // absolute path of the signed pomar-host
	Guest   Guest
	Procs   proc.Lister
	// Mirrors resolves and snapshots attempt sources; nil disables sources.
	Mirrors *mirror.Mirrors
	// Base returns a verified base rootfs for helpers to clone, or "" to have
	// them unpack the image. Nil means always unpack.
	Base func() (string, error)
	UID  int
	// Class is the job class every attempt is admitted as; zero means
	// capacity.CI.
	Class capacity.Class
	// Host is what attempts may claim in total; zero reads this host.
	Host capacity.Host
	// Space reads the data root's filesystem at claim time; nil reads it
	// from the venue.
	Space func() (venue.Space, error)
	// GoProxy is served to each attempt with a source on its own socket,
	// and ShimBin (a static linux/arm64 pomar-shim) is copied into its guest
	// to reach it; both or neither.
	GoProxy *goproxy.Proxy
	ShimBin string
	// Budgets bound the caches; nil means cache.Budgets.
	Budgets []cache.Budget
	// Pinned returns absolute paths the running configuration boots from
	// (the kernel, the base); the caches holding them are never evicted.
	Pinned func() []string
	// ReapWait bounds how long an orphan gets between SIGTERM and SIGKILL.
	ReapWait time.Duration
	// Poll is the liveness check interval.
	Poll time.Duration
	// StallWait is how long a live attempt may show no progress on a full
	// host before it is stopped as host-disk-full.
	StallWait time.Duration
	// SampleEvery is how often a live attempt's VM service is measured;
	// Footprint reads its memory (nil runs footprint(1)).
	SampleEvery time.Duration
	Footprint   Footprinter
	Log         io.Writer
	// CtlSocket, when set, is a second socket for the stream (DESIGN-02
	// approach A; r14 condition 4). It is created at mode 0660 in a directory
	// the operator makes the manager's user's, group pomarctl, mode 0750, and
	// takes its group from that directory. It serves the stream's routes only:
	// start, stop and reads; never a route that removes a record, evicts a
	// cache or changes configuration.
	CtlSocket string
	// Signer signs each terminal attempt's result document; nil writes the
	// document unsigned. Only the role user's permanent manager has one (the
	// signing-identity design, approach A); the stream's never does.
	Signer *result.Signer
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
	// seen is poll's own: when it first saw each live helper.
	seen map[string]time.Time
	// proxies are the live attempts' module proxy listeners.
	proxies map[string]*proxyListener
	// vmOrphans are the VM services no live attempt accounts for.
	vmOrphans map[int]*VMOrphan
	lastSweep time.Time
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
	if cfg.SampleEvery == 0 {
		cfg.SampleEvery = 2 * time.Second
	}
	if cfg.Footprint == nil {
		cfg.Footprint = FootprintTool
	}
	if cfg.StallWait == 0 {
		cfg.StallWait = 2 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Class == (capacity.Class{}) {
		cfg.Class = capacity.CI
	}
	if cfg.Host == (capacity.Host{}) {
		h, err := capacity.ReadHost()
		if err != nil {
			return nil, err
		}
		cfg.Host = h
	}
	if cfg.Budgets == nil {
		cfg.Budgets = cache.Budgets
	}
	if cfg.Space == nil {
		cfg.Space = v.Space
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
	m := &Manager{cfg: cfg, dir: dir, lock: lock, t: t, events: ev, done: make(chan struct{}), seen: map[string]time.Time{}, proxies: map[string]*proxyListener{}, vmOrphans: map[int]*VMOrphan{}}
	m.event("manager-start", "", 0, "")
	if err := m.reconcile(); err != nil {
		m.Close()
		return nil, err
	}
	m.reopenProxies()
	m.evict()
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
	defer m.orphanSweep(nil, true) // after the findings are applied
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

// Source names the code an attempt runs: a mirror and a ref. The ref is
// pinned to a commit SHA at admission, and the attempt only ever sees a
// snapshot of that commit.
type Source struct {
	Mirror string `json:"mirror"`
	Ref    string `json:"ref"`
	// Git gives the guest a repository instead of a tree: a bundle of Ref
	// (a branch) and of Base, fetched with no remote into /work and checked
	// out at the pinned commit. Jobs that read their own history need it.
	Git  bool   `json:"git,omitempty"`
	Base string `json:"base,omitempty"` // default "main"
}

// Start ledgers an attempt's objects and spawns its helper in a new session,
// so that the manager's own death does not signal it. With a source, the ref
// is pinned to a commit SHA before anything is created, and a snapshot of
// that commit is written into the attempt's record.
func (m *Manager) Start(id string, command []string, src *Source, inputs ...Input) (Entry, error) {
	v := m.cfg.Venue
	if !validID.MatchString(id) {
		return Entry{}, fmt.Errorf("manager: invalid attempt id %q", id)
	}
	if len(command) == 0 {
		return Entry{}, errors.New("manager: empty command")
	}
	if src != nil && m.cfg.Mirrors == nil {
		return Entry{}, errors.New("manager: sources are not configured")
	}
	if len(inputs) > 0 && src == nil {
		return Entry{}, errors.New("manager: inputs need a source: they are released with it")
	}
	inputRecs, err := checkInputs(inputs)
	if err != nil {
		return Entry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.t.entries[id]; ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrExists, id)
	}
	// Claim-time admission, under the lock so that two starts cannot both
	// take the last slot. Nothing is created before it passes.
	class := m.cfg.Class
	if err := m.admit(class); err != nil {
		m.event("start-refused", id, 0, err.Error())
		return Entry{}, err
	}
	// Refuse an unentitled helper binary before creating anything.
	if err := sign.Check(m.cfg.HostBin); err != nil {
		return Entry{}, err
	}
	// The pins this attempt runs with, read now: the helper binary as it is
	// at this moment, which is what the helper will execute.
	pins, err := m.pinsFor(command)
	if err != nil {
		return Entry{}, err
	}
	var basePath string
	if m.cfg.Base != nil {
		var err error
		if basePath, err = m.cfg.Base(); err != nil {
			return Entry{}, err
		}
	}
	ctx := context.Background()
	var sha string
	if src != nil {
		var err error
		if sha, err = m.cfg.Mirrors.Resolve(ctx, src.Mirror, src.Ref); err != nil {
			return Entry{}, err
		}
	}
	recRel := filepath.Join(attemptsDir, id)
	recID := "attempt-" + id
	if err := v.Intent(venue.KindVolume, venue.ClassAttempt, recID, recRel, "attempt record"); err != nil {
		return Entry{}, err
	}
	rec := filepath.Join(v.Root(), recRel)
	// Every failure from here on tears down what this call created.
	abandon := func(err error, vmOpen bool) (Entry, error) {
		if vmOpen {
			v.Failed(venue.KindVM, id, err.Error())
			v.Teardown(venue.KindVM, id)
		}
		m.closeProxy(id)
		v.Failed(venue.KindVolume, recID, err.Error())
		v.Teardown(venue.KindVolume, recID)
		m.event("start-refused", id, 0, err.Error())
		return Entry{}, err
	}
	if err := os.MkdirAll(rec, 0o700); err != nil {
		return abandon(err, false)
	}
	if err := v.Created(venue.KindVolume, recID); err != nil {
		return abandon(err, false)
	}
	if src != nil {
		if err := m.writeSource(ctx, src, sha, rec); err != nil {
			return abandon(err, false)
		}
		if _, err := writeInputs(rec, inputs); err != nil {
			return abandon(err, false)
		}
		b, _ := json.Marshal(map[string]any{"mirror": src.Mirror, "ref": src.Ref, "sha": sha, "git": src.Git, "base": src.base()})
		if err := os.WriteFile(filepath.Join(rec, "source.json"), append(b, '\n'), 0o600); err != nil {
			return abandon(err, false)
		}
	}
	if err := v.Intent(venue.KindVM, venue.ClassAttempt, id, filepath.Join(storeDir, "containers", id), "helper-owned guest"); err != nil {
		return abandon(err, false)
	}

	g := m.cfg.Guest
	args := []string{"helper", "--attempt", id, "--state-dir", rec,
		"--store", filepath.Join(v.Root(), storeDir), "--kernel", g.Kernel,
		"--init", g.InitRef, "--init-digest", g.InitDigest,
		"--image", g.ImageRef, "--image-digest", g.ImageDigest,
		"--cpus", strconv.Itoa(class.VCPU), "--memory-bytes", strconv.FormatInt(class.MemoryBytes, 10)}
	if basePath != "" {
		args = append(args, "--base", basePath)
	}
	if src != nil {
		// The helper copies the pinned snapshot into the guest over vsock
		// before releasing the command; the guest never sees the mirror.
		args = append(args, "--source", filepath.Join(rec, src.file()), "--source-kind", src.kind(), "--source-sha", sha)
		if len(inputs) > 0 {
			args = append(args, "--inputs", filepath.Join(rec, inputsDir))
		}
	}
	withProxy := src != nil && m.proxyEnabled()
	if withProxy {
		// Its own socket, relayed into its guest only; the shim serves it on
		// the guest's loopback as GOPROXY.
		if err := m.listenProxy(id); err != nil {
			return abandon(err, true)
		}
		args = append(args, "--goproxy-socket", m.proxySocket(id), "--shim", m.cfg.ShimBin)
	}
	args = append(append(args, "--"), command...)
	logf, err := os.OpenFile(filepath.Join(rec, "helper.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return abandon(err, true)
	}
	cmd := exec.Command(m.cfg.HostBin, args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(v.Root(), tmpDir)+string(filepath.Separator))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return abandon(err, true)
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
		m.closeProxy(id)
		m.event("start-error", id, pid, err.Error())
		return Entry{}, err
	}
	e := &Entry{Attempt: id, Command: command, PID: pid, Start: start, State: StateStarting, Created: time.Now().UTC(), Class: class, GoProxy: withProxy, Pins: pins, Inputs: inputRecs}
	if src != nil {
		e.Source = &PinnedSource{Mirror: src.Mirror, Ref: src.Ref, SHA: sha, Git: src.Git}
		if src.Git {
			e.Source.Base = src.base()
		}
	}
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

// admit decides a claim against the live attempts. m.mu must be held.
func (m *Manager) admit(c capacity.Class) error {
	sp, err := m.cfg.Space()
	if err != nil {
		return err
	}
	var live []capacity.Class
	for _, e := range m.t.entries {
		if !e.Terminal() {
			live = append(live, e.claimClass())
		}
	}
	return capacity.Admit(c, live, m.cfg.Host, capacity.Disk{Used: sp.Used, Avail: sp.Avail, Shared: sp.Shared})
}

// Capacity is the manager's admission state, for status output.
type Capacity struct {
	Class capacity.Class `json:"class"`
	Host  capacity.Host  `json:"host"`
	Space venue.Space    `json:"space"`
	Live  int            `json:"live"`
	// FitsIdle is how many attempts of the class fit on this host when
	// idle. It is a plan figure only when the class is measured.
	FitsIdle int `json:"fits_idle"`
}

// Capacity reports the admission state.
func (m *Manager) Capacity() (Capacity, error) {
	sp, err := m.cfg.Space()
	if err != nil {
		return Capacity{}, err
	}
	m.mu.Lock()
	live := 0
	for _, e := range m.t.entries {
		if !e.Terminal() {
			live++
		}
	}
	m.mu.Unlock()
	d := capacity.Disk{Used: sp.Used, Avail: sp.Avail, Shared: sp.Shared}
	return Capacity{Class: m.cfg.Class, Host: m.cfg.Host, Space: sp, Live: live,
		FitsIdle: capacity.Fits(m.cfg.Class, m.cfg.Host, d)}, nil
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
	return m.removeLocked(id, "")
}

// removeLocked is Remove with m.mu held and a ledger note for the removal.
func (m *Manager) removeLocked(id, note string) error {
	e, ok := m.t.entries[id]
	if !ok {
		return fmt.Errorf("manager: no attempt %s", id)
	}
	if !e.Terminal() {
		return fmt.Errorf("manager: attempt %s is %s; stop it first", id, e.State)
	}
	v := m.cfg.Venue
	if v.IsOpen(venue.KindVolume, "attempt-"+id) {
		if err := v.TeardownNote(venue.KindVolume, "attempt-"+id, note); err != nil {
			return err
		}
	}
	delete(m.t.entries, id)
	m.event("removed", id, 0, note)
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
		m.poll()
	}
}

// poll checks every live entry once. The entries are copied before the
// processes are listed, so every entry judged was spawned before the listing
// it is judged against: a listing taken first could miss a helper that Start
// spawned while the watcher waited for the lock. An end is recorded only
// when a second, fresh listing agrees.
func (m *Manager) poll() {
	m.mu.Lock()
	var live []Entry
	for _, e := range m.t.entries {
		if !e.Terminal() {
			live = append(live, *e)
		}
	}
	m.mu.Unlock()
	if len(live) == 0 {
		m.orphanSweep(nil, false)
		return
	}
	ps, err := m.cfg.Procs.List()
	if err != nil {
		return // unknown: change nothing
	}
	m.noteSeen(live, ps)
	m.orphanSweep(ps, false)
	m.mu.Lock()
	m.linkVMs(ps)
	if m.samplePeaks(ps, time.Now()) {
		m.t.save()
	}
	m.mu.Unlock()
	m.checkDiskFull(live)
	for _, e := range live {
		if m.isHelperOf(ps, e) {
			m.observe(e.Attempt)
			continue
		}
		again, err := m.cfg.Procs.List()
		if err != nil || m.isHelperOf(again, e) {
			continue
		}
		st, reason, code := m.terminalFromStatus(e.Attempt)
		m.finish(e.Attempt, st, reason, code)
	}
}

// markHostCondition records that condition held on the host while attempt
// id was live. The first condition recorded is kept.
func (m *Manager) markHostCondition(id, condition string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.t.entries[id]; e != nil && !e.Terminal() && e.HostCondition == "" {
		e.HostCondition = condition
		m.t.save()
		m.event(condition, id, e.PID, "")
	}
}

// markDiskFull records the first host condition: the data root's filesystem
// was full (under capacity.DiskFullBytes free) while the attempt was live.
func (m *Manager) markDiskFull(id string) { m.markHostCondition(id, capacity.ReasonHostDiskFull) }

// checkDiskFull samples the data root's free space for the live entries. On
// a full host it marks them all, and stops any whose helper has made no
// progress for StallWait: a paused or hung guest never ends by itself. A
// helper that ignores the stop is killed after a second StallWait.
func (m *Manager) checkDiskFull(live []Entry) {
	sp, err := m.cfg.Space()
	if err != nil || sp.Avail >= capacity.DiskFullBytes {
		return
	}
	for _, e := range live {
		m.markDiskFull(e.Attempt)
		idle := time.Since(m.lastProgress(e.Attempt))
		if idle < m.cfg.StallWait {
			continue
		}
		m.mu.Lock()
		cur := m.t.entries[e.Attempt]
		if cur == nil || cur.Terminal() || !m.sameHelperAlive(cur.PID, cur.Attempt) {
			m.mu.Unlock()
			continue
		}
		sig, what := syscall.SIGTERM, "stall-sigterm"
		if cur.State == StateStopping && idle >= 2*m.cfg.StallWait {
			sig, what = syscall.SIGKILL, "stall-sigkill"
		} else if cur.State == StateStopping {
			m.mu.Unlock()
			continue
		}
		syscall.Kill(cur.PID, sig)
		cur.State = StateStopping
		m.t.save()
		m.mu.Unlock()
		m.event(what, e.Attempt, e.PID, fmt.Sprintf("host full; no progress for %s", idle.Round(time.Second)))
	}
}

// noteSeen records when poll first saw each live helper, and forgets ended
// ones. The first sighting starts the stall clock: a helper adopted after a
// restart gets a full StallWait before it is judged.
func (m *Manager) noteSeen(live []Entry, ps []proc.Process) {
	seen := map[string]time.Time{}
	for _, e := range live {
		if !m.isHelperOf(ps, e) {
			continue
		}
		if t, ok := m.seen[e.Attempt]; ok {
			seen[e.Attempt] = t
		} else {
			seen[e.Attempt] = time.Now()
		}
	}
	m.seen = seen
}

// lastProgress is the newest sign of life from an attempt: a change to its
// status or output, or else its first sighting. The helper's own CPU time
// is no sign: the guest runs in Virtualization's XPC service, not in the
// helper.
func (m *Manager) lastProgress(id string) time.Time {
	t := m.seen[id]
	for _, f := range []string{"status.json", "output.log"} {
		if fi, err := os.Stat(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, f)); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	return t
}

// isHelperOf reports whether ps shows e's own helper: its pid, start time,
// uid, executable and attempt.
func (m *Manager) isHelperOf(ps []proc.Process, e Entry) bool {
	p, ok := proc.Find(ps, e.PID)
	if !ok || p.Start != e.Start || p.UID != m.cfg.UID {
		return false
	}
	a, isHelper := helperAttempt(p.Args, m.cfg.HostBin)
	return isHelper && a == e.Attempt
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
	// The helper reads the free space as its guest ends, before deleting
	// the guest frees the clone; a full host then marks the entry.
	if free, err := strconv.ParseInt(s["host_free_bytes"], 10, 64); err == nil && free >= 0 && free < capacity.DiskFullBytes {
		m.markDiskFull(id)
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
	// A compromised host fails the attempt under the condition's name,
	// whatever shape the failure took. Even an exit 0 is failed: on the
	// bounded volume the guest's fsync got EIO and a command that went on
	// exited 0, so a result produced while the host could not honour the
	// guest is not a result. The exit code is kept.
	if e.HostCondition != "" {
		detail := string(st)
		if code != nil {
			detail += fmt.Sprintf(" %d", *code)
		}
		if reason != "" {
			detail += ": " + reason
		}
		st, reason = StateFailed, e.HostCondition+" ("+detail+")"
	}
	e.State, e.Reason, e.ExitCode, e.Ended = st, reason, code, time.Now().UTC()
	m.closeProxy(id)
	m.t.save()
	final := *e
	m.mu.Unlock()
	m.event("ended-"+string(st), id, e.PID, reason)
	m.writeResult(final)
	m.teardownVM(id)
	m.evict()
}

func (s *Source) base() string {
	if s.Base == "" {
		return "main"
	}
	return s.Base
}

func (s *Source) kind() string {
	if s.Git {
		return "bundle"
	}
	return "tar"
}

func (s *Source) file() string {
	if s.Git {
		return "source.bundle"
	}
	return "source.tar"
}

// writeSource puts the attempt's source in its record: a tar of the pinned
// tree, or with Git a bundle of the branch and base that holds the pinned
// commit.
func (m *Manager) writeSource(ctx context.Context, src *Source, sha, rec string) error {
	dst := filepath.Join(rec, src.file())
	if src.Git {
		return m.cfg.Mirrors.Bundle(ctx, src.Mirror, sha, src.Ref, src.base(), dst)
	}
	return m.cfg.Mirrors.Snapshot(ctx, src.Mirror, sha, dst)
}
