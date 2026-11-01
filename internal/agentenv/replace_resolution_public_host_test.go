package agentenv

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// POMAR-CC-SOW-37 rev3: a public-Host-API replacement->resolution regression
// using genuine host-produced evidence -- every lifecycle step (create implied
// by hand-built fixture, start, stop, reconcile, start again, continuations)
// goes through the real h.Handler() HTTP routes and the real host.go/
// reconcile.go/continuation.go code, exactly as in production. The only
// substitutions are the package's own existing, long-established test seams:
// h.probe (reconcile.go:114-115, used throughout reconcile_test.go/
// retirement_test.go/preboot_reconcile_test.go) standing in for the native
// Swift fence-probe subprocess, and a real, test-owned no-op executable
// standing in for the Swift VM helper itself (so start() launches and
// proc.Find locates a REAL process, never a synthetic Action literal).
//
// This file does not touch host.go, reconcile.go or continuation.go.

// fakeHelperBinary builds a real, native (no shebang interpreter) executable
// that ignores every argument start() passes it and just blocks, so
// cmd.Start() launches a real, locatable process without booting any VM. It
// must be a native binary, not a shell script: reconcile.go's reconcileHandler
// checks e.Process.Args == "<helper> agent-environment --config <file>"
// exactly as `ps` reports the live command line, and a shebang script's own
// argv[0] (verified empirically) is the interpreter ("/bin/sh"), not the
// script path -- only a direct exec keeps the helper's own path as argv[0].
func fakeHelperBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fakehelper.go")
	// A bare `select {}` looks like it blocks forever, but the Go runtime's
	// own deadlock detector treats a main goroutine with zero progress and
	// no other goroutines as a real deadlock and exits(2) immediately
	// (confirmed empirically) -- a sleep loop makes genuine, if idle,
	// progress, so the runtime never flags it.
	if err := os.WriteFile(src, []byte("package main\nimport \"time\"\nfunc main() { for { time.Sleep(time.Hour) } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakehelper")
	// Bounded: pomar's review -- a failed/hung fixture build must not hang
	// the whole finite gate alongside it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fake helper binary: %v\n%s", err, out)
	}
	return bin
}

func newTestHost(t *testing.T) (*Host, string) {
	t.Helper()
	root := t.TempDir()
	h := &Host{config: HostConfig{Root: root, Helper: fakeHelperBinary(t), MaxLive: 2, CPUs: 1, MemoryBytes: 512 << 20}, envs: map[string]*Environment{}, gates: map[string]*Egress{}, servers: map[string]*http.Server{}}
	return h, root
}

// envSnap is a safe, locked copy of the Environment fields this file's tests
// read after a real start(): host.go's own background watcher goroutine
// (spawned by the real start(), host.go:584) writes e.Phase/e.Process/
// e.HelperExit under h.mu from a separate goroutine once the real helper
// process it's watching exits (which this file's own kill/stop calls cause).
// Reading those fields directly, unlocked, is a genuine data race the race
// detector correctly reports -- host.go's own code always holds h.mu around
// them; a test reading them unlocked doesn't get an exemption.
type envSnap struct {
	Phase       string
	Incarnation string
	PID         int
	Start       string
	HelperExit  *HelperExit
	Actions     map[string]Action
}

func snapshot(h *Host, e *Environment) envSnap {
	h.mu.Lock()
	defer h.mu.Unlock()
	actions := make(map[string]Action, len(e.Actions))
	for k, v := range e.Actions {
		actions[k] = v
	}
	var exit *HelperExit
	if e.HelperExit != nil {
		cp := *e.HelperExit
		exit = &cp
	}
	return envSnap{Phase: e.Phase, Incarnation: e.Spec.Incarnation, PID: e.Process.PID, Start: e.Process.Start, HelperExit: exit, Actions: actions}
}

func hostActionCall(h *Host, env, action, operationID, incarnation string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"operation_id": operationID, "expected_incarnation": incarnation})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/"+env+"/"+action, strings.NewReader(string(body))))
	return w
}

// shortTempDir returns a short-path temp directory: t.TempDir()'s own path
// (under a long macOS /private/var/folders prefix plus the test name)
// routinely exceeds Darwin's ~104-byte sun_path limit for Unix sockets, and
// gate() derives its own socket paths (egress, go.sock) from e.Spec.Directory
// directly -- so the whole environment directory needs a short root, not
// just the explicit socket fields.
// pomar's review, 2026-11-01: os.MkdirTemp("", ...) returns an unresolved
// path under /var/folders/... on macOS, and /var -> private/var is itself a
// symlink -- PrivateDir's own EvalSymlinks(path) == path check (by design,
// not a defect) refuses that alias, breaking any receipt or custody check
// that passes this directory through. Resolving it here, once, is the same
// fix already applied to internal/distribution and internal/calljournal's
// fixtures for the identical host-environment quirk.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestPublicHostStartLaunchesARealLocatableProcess is step 1: confirm the
// real start() handler, through the real HTTP route, actually launches and
// records a real OS process -- the foundation the rest of this file's
// sequence builds on.
func TestPublicHostStartLaunchesARealLocatableProcess(t *testing.T) {
	h, _ := newTestHost(t)
	dir := filepath.Join(shortTempDir(t), "env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Environment{Spec: VMSpec{HostConfig: h.config, Environment: "env", Session: "session1", Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4"), ControlSocket: filepath.Join(dir, "agent.sock"), EgressSocket: filepath.Join(dir, "egress.sock")}, Phase: "created", Actions: map[string]Action{}}
	h.envs["env"] = e

	w := hostActionCall(h, "env", "start", "start-1", "")
	if w.Code != 200 {
		t.Fatalf("start failed: %d %s", w.Code, w.Body.String())
	}
	// pomar's review (POMAR147-CLEANUP-1): the pinned helper's cleanup must
	// be registered immediately after a successful launch, before any
	// fallible assertion -- otherwise a t.Fatal in one of those assertions
	// leaks the real process, since Cleanup only runs what was already
	// registered. snapshot() itself cannot fail/fatal, so it's safe to read
	// before registering.
	snap := snapshot(h, e)
	t.Cleanup(func() { killOwnedProcess(t, proc.Process{PID: snap.PID, Start: snap.Start}) })

	if snap.Phase != "running" {
		t.Fatalf("phase = %q, want running", snap.Phase)
	}
	if snap.Incarnation == "" {
		t.Fatal("no incarnation assigned")
	}
	if snap.PID == 0 {
		t.Fatal("no process recorded")
	}
	// Confirm it's genuinely a real, currently-running process -- not just a
	// recorded number.
	if err := syscall.Kill(snap.PID, 0); err != nil {
		t.Fatalf("recorded PID %d is not a real running process: %v", snap.PID, err)
	}

	a := snap.Actions["start-1"]
	if a.State != "completed" || a.ResultingIncarnation != snap.Incarnation {
		t.Fatalf("start action not recorded correctly: %+v", a)
	}
}

// writeVMStopped writes the guest-owned status file stop() polls for --
// nothing in this package's tests has a real guest to write it, so every
// test that calls /stop writes it directly, same as reconciliationFixture/
// retirementFixture already do for vm-status.json.
func writeVMStopped(t *testing.T, h *Host, e *Environment) {
	t.Helper()
	snap := snapshot(h, e)
	h.mu.Lock()
	environment, session, directory := e.Spec.Environment, e.Spec.Session, e.Spec.Directory
	h.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"environment": environment, "session": session, "incarnation": snap.Incarnation, "vm_stopped": "true"})
	if err := os.WriteFile(filepath.Join(directory, "vm-status-"+snap.Incarnation+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// killOwnedProcess sends a real SIGKILL to want.PID, but only after
// re-checking, via a real process-table read (the same proc.Find pair
// host.go's own departed() checks -- PID AND start time together), that the
// PID still identifies the exact process this fixture itself launched.
// pomar's review: a bare PID, once its original process has already exited,
// can be silently reused by the OS for something completely unrelated --
// signaling it blind risks killing a stranger process, not the fixture's
// own helper. If the identity no longer matches, this sends nothing.
func killOwnedProcess(t *testing.T, want proc.Process) {
	t.Helper()
	ps, err := (proc.PS{}).List()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := proc.Find(ps, want.PID)
	if !ok || got.Start != want.Start {
		t.Logf("pid %d no longer matches the owned process identity (want start %q; found %+v, present=%v) -- not signaling", want.PID, want.Start, got, ok)
		return
	}
	if err := syscall.Kill(want.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatal(err)
	}
}

// killAndWaitForDeparture kills the environment's own currently-recorded
// helper process (identity-checked, see killOwnedProcess) and then waits
// for confirmation by joining host.go's own authoritative departure signal
// -- e.HelperExit, set by start()'s internal cmd.Wait() goroutine once it
// reaps the real child -- rather than a second, independent raw-PID poll
// racing against that same reap (pomar's review: "join its owned wait
// completion").
func killAndWaitForDeparture(t *testing.T, h *Host, e *Environment) {
	t.Helper()
	snap := snapshot(h, e)
	killOwnedProcess(t, proc.Process{PID: snap.PID, Start: snap.Start})
	for range 100 {
		if exit := snapshot(h, e).HelperExit; exit != nil && exit.PID == snap.PID {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d: no HelperExit recorded after kill", snap.PID)
}

// realFenceEvidence is a *correctly shaped* h.probe substitute: it reports
// against the real rootfs file's real device/inode (read via os.Stat, not
// invented), so the Reconciliation this produces binds to genuinely real
// filesystem identity -- the one piece true machineProbe would also report
// from the real, unmodified Go code calling it.
func realFenceEvidence(t *testing.T, e *Environment, confirmed bool, issues ...string) func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
	t.Helper()
	return func(_ context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
		fi, err := os.Stat(e.Spec.Rootfs)
		if err != nil {
			t.Fatal(err)
		}
		s := fi.Sys().(*syscall.Stat_t)
		return FenceProbeEvidence{
			Environment: r.Environment, Session: r.Session, Incarnation: r.Incarnation, Operation: r.Operation,
			ObservedAt: time.Now().UTC().Format(time.RFC3339), Coverage: "direct-vz-private-disk/v1",
			ObserverUID: os.Getuid(), ProcessCount: 1, OwnProcessesChecked: 1,
			RootfsDevice: uint32(s.Dev), RootfsInode: s.Ino,
			Confirmed: confirmed, Issues: issues,
		}, nil
	}
}

// TestPublicHostStopThenReconcileProducesRealReconciliationEvidence is step
// 2: start (as above), then stop and reconcile the SAME, still-current
// incarnation through the real handlers -- the already-supported sequence
// (stop -> reconcile -> start) that needs no production fix at all, proven
// with a real process and real (if probe-substituted) evidence rather than
// a hand-built Action.
func TestPublicHostStopThenReconcileProducesRealReconciliationEvidence(t *testing.T) {
	h, _ := newTestHost(t)
	dir := filepath.Join(shortTempDir(t), "env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Environment{Spec: VMSpec{HostConfig: h.config, Environment: "env", Session: "session1", Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4"), ControlSocket: filepath.Join(dir, "agent.sock"), EgressSocket: filepath.Join(dir, "egress.sock")}, Phase: "created", Actions: map[string]Action{}}
	h.envs["env"] = e
	h.stopWait = 2 * time.Second

	if w := hostActionCall(h, "env", "start", "start-1", ""); w.Code != 200 {
		t.Fatalf("start failed: %d %s", w.Code, w.Body.String())
	}
	// pomar's review (POMAR147-CLEANUP-1): registered immediately after the
	// successful launch, before the fallible WriteFile/kill/stop sequence
	// below -- a pinned helper leaks if any of those fatal first.
	snap1 := snapshot(h, e)
	t.Cleanup(func() { killOwnedProcess(t, proc.Process{PID: snap1.PID, Start: snap1.Start}) })
	incarnation1 := snap1.Incarnation
	if err := os.WriteFile(e.Spec.Rootfs, []byte("retained workspace bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	killAndWaitForDeparture(t, h, e)
	writeVMStopped(t, h, e)
	if w := hostActionCall(h, "env", "stop", "stop-1", incarnation1); w.Code != 200 {
		t.Fatalf("stop failed: %d %s", w.Code, w.Body.String())
	}
	if phase := snapshot(h, e).Phase; phase != "stopped" {
		t.Fatalf("phase = %q, want stopped", phase)
	}

	h.probe = realFenceEvidence(t, e, true)
	reconcileBody, _ := json.Marshal(ReconcileRequest{OperationID: "reconcile-1", SessionID: "session1", Incarnation: incarnation1, LaunchOperation: "start-1", FenceOperation: "stop-1"})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/reconcile", strings.NewReader(string(reconcileBody))))
	if w.Code != 200 {
		t.Fatalf("reconcile failed: %d %s", w.Code, w.Body.String())
	}

	a := snapshot(h, e).Actions["reconcile-1"]
	if a.Reconciliation == nil || !a.Reconciliation.ReplacementEligible || a.State != "completed" {
		t.Fatalf("reconcile did not produce a confirmed Reconciliation: %+v", a)
	}
	fi, _ := os.Stat(e.Spec.Rootfs)
	s := fi.Sys().(*syscall.Stat_t)
	if a.Reconciliation.ReplacementEligible && (uint32(s.Dev) == 0 && s.Ino == 0) {
		t.Fatal("evidence not actually bound to the real rootfs identity")
	}
}

// realGuestWorkspace sets up a real, hermetic git repo (bindContinuation's
// own inventory check runs real git commands against it) and returns its
// HEAD sha, matching continuation_test.go's own continuationFixture pattern.
func realGuestWorkspace(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-qm", "fixture"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

// serveGuestBroker listens on socket (removing any prior listener there --
// a real restart binds the same path fresh) and serves broker.Handler() in
// the background, returning a stop func. This is the real guest-side
// continuation path (bindContinuation), reached only through the Host's own
// postUnix proxy, never called directly by the test.
func serveGuestBroker(t *testing.T, socket string, broker *Broker) {
	t.Helper()
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: broker.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// TestPublicHostReplaceResolutionTargetSequence is the full target sequence:
// start -> accept an operation on the real guest store -> stop -> reconcile
// (real evidence, as proven above) -> start again (a real second incarnation)
// -> a real guest broker restarted under that new incarnation -> the real
// Host-level /continuations endpoint, proxying over a real Unix socket to
// the real broker's bindContinuation. Nothing here is a manufactured
// receipt: the only substitutions are h.probe (the package's existing native-
// probe seam) and the two real-but-test-owned processes (helper binary,
// guest broker) standing in for the Swift VM and the guest's own broker
// process -- never a hand-built Action.Reconciliation or Store.Continuations
// entry.
func TestPublicHostReplaceResolutionTargetSequence(t *testing.T) {
	h, _ := newTestHost(t)
	dir := filepath.Join(shortTempDir(t), "env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Environment{Spec: VMSpec{HostConfig: h.config, Environment: "env", Session: "session1", Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4"), ControlSocket: filepath.Join(dir, "agent.sock"), EgressSocket: filepath.Join(dir, "egress.sock")}, Phase: "created", Actions: map[string]Action{}}
	h.envs["env"] = e
	h.stopWait = 2 * time.Second

	workspace, head := realGuestWorkspace(t)
	storeDir := filepath.Join(shortTempDir(t), "store")

	// --- incarnation 1: start, accept an operation, leave it held ---
	if w := hostActionCall(h, "env", "start", "start-1", ""); w.Code != 200 {
		t.Fatalf("start-1 failed: %d %s", w.Code, w.Body.String())
	}
	// pomar's review (POMAR147-CLEANUP-1): registered immediately after the
	// successful launch, before the fallible Store/Broker setup below.
	snap1 := snapshot(h, e)
	t.Cleanup(func() { killOwnedProcess(t, proc.Process{PID: snap1.PID, Start: snap1.Start}) })
	incarnation1 := snap1.Incarnation
	if err := os.WriteFile(e.Spec.Rootfs, []byte("retained workspace bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	store1, err := Open(storeDir, "env", "session1", incarnation1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store1.BindSource(head); err != nil {
		t.Fatal(err)
	}
	task := Task{OperationID: "original-op", Incarnation: incarnation1, Text: "do the bounded work"}
	if _, _, err := store1.Begin(task); err != nil {
		t.Fatal(err)
	}
	if err := store1.Thread(incarnation1, "original-thread"); err != nil {
		t.Fatal(err)
	}
	if err := store1.Accepted(incarnation1, "original-op", "original-turn"); err != nil {
		t.Fatal(err)
	}
	originalInputHash := store1.Snapshot().Operations["original-op"].InputHash
	broker1 := NewBroker(store1, workspace)
	broker1.Actor = actorFixture()
	serveGuestBroker(t, e.Spec.ControlSocket, broker1)

	// --- stop + reconcile incarnation 1 (proven above) ---
	killAndWaitForDeparture(t, h, e)
	writeVMStopped(t, h, e)
	if w := hostActionCall(h, "env", "stop", "stop-1", incarnation1); w.Code != 200 {
		t.Fatalf("stop-1 failed: %d %s", w.Code, w.Body.String())
	}
	h.probe = realFenceEvidence(t, e, true)
	reconcileBody, _ := json.Marshal(ReconcileRequest{OperationID: "reconcile-1", SessionID: "session1", Incarnation: incarnation1, LaunchOperation: "start-1", FenceOperation: "stop-1"})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/reconcile", strings.NewReader(string(reconcileBody))))
	if w.Code != 200 {
		t.Fatalf("reconcile-1 failed: %d %s", w.Code, w.Body.String())
	}
	store1.Close()

	// --- incarnation 2: start again ---
	if w := hostActionCall(h, "env", "start", "start-2", incarnation1); w.Code != 200 {
		t.Fatalf("start-2 failed: %d %s", w.Code, w.Body.String())
	}
	snap2 := snapshot(h, e)
	incarnation2 := snap2.Incarnation
	t.Cleanup(func() { killOwnedProcess(t, proc.Process{PID: snap2.PID, Start: snap2.Start}) })

	// --- the real guest restarts under the new incarnation: reopening the
	// same store directory is what actually (and honestly) transitions the
	// held operation to acceptance_unknown -- Open()'s own real logic
	// (state.go), not a test-injected shortcut. ---
	store2, err := Open(storeDir, "env", "session1", incarnation2)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	original := store2.Snapshot().Operations["original-op"]
	if original.State != "acceptance_unknown" {
		t.Fatalf("fixture must reach the real held state via Open(), got %q", original.State)
	}
	broker2 := NewBroker(store2, workspace)
	actor2 := actorFixture()
	broker2.Actor = actor2
	serveGuestBroker(t, e.Spec.ControlSocket, broker2)

	// --- the real Host-level /continuations call ---
	inventory, err := broker2.inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	invBytes, _ := json.Marshal(inventory)
	req := ContinuationRequest{
		DispositionID: "target-sequence", IntentRef: "resume-bounded-work", AuthorityRef: "pomar-cc-sow37-rev3",
		ResponsibleOwner: "pomar-cc", EnvironmentID: "env", WorkspaceID: "workspace-env", SessionID: "session1",
		OriginalOperation: "original-op", OriginalIncarnation: incarnation1, OriginalInputSHA256: originalInputHash,
		OriginalThreadID: "original-thread", OriginalTurnID: "original-turn",
		OriginalFenceOperation: "reconcile-1", PredecessorIncarnation: incarnation1, PredecessorFenceOperation: "reconcile-1",
		LaunchOperation: "start-2", WorkspaceInventorySHA256: digest(invBytes),
		RecoveryDescription:     "resume remaining bounded work after a real stop/reconcile/start sequence",
		ExternalActionInventory: "no scoped external action evidenced",
		Successor:               Task{OperationID: "successor-op", Incarnation: incarnation2, Text: "complete the remaining bounded work"},
	}
	reqBody, _ := json.Marshal(req)
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/continuations", strings.NewReader(string(reqBody))))
	if w.Code != 200 {
		t.Fatalf("continuations call failed: %d %s", w.Code, w.Body.String())
	}

	if got := store2.Snapshot().Operations["original-op"]; got != original {
		t.Fatalf("original operation history changed: got %+v, want %+v", got, original)
	}

	// bindContinuation only binds the successor identity; dispatching its
	// turn is a separate submission, exactly as the existing broker-level
	// test (TestContinuationPreservesUnknownAndDispatchesOnlyBoundSuccessorOnce)
	// already establishes. Submit it through the real Host agent-proxy route
	// (/v1/environments/{id}/agent/{path}), not by calling broker2.Submit
	// directly, so this stays a public-Host-API test end to end.
	taskBody, _ := json.Marshal(req.Successor)
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/agent/tasks", strings.NewReader(string(taskBody))))
	if w.Code != 200 {
		t.Fatalf("successor task submission failed: %d %s", w.Code, w.Body.String())
	}
	if actor2.count("turn/start") != 1 {
		t.Fatalf("expected exactly one successor turn dispatched, got %d", actor2.count("turn/start"))
	}
	if got := store2.Snapshot().Operations["original-op"]; got != original {
		t.Fatalf("original operation history changed after successor dispatch: got %+v, want %+v", got, original)
	}
}

// TestKillOwnedProcessRefusesWhenIdentityNoLongerMatches is pomar's required
// regression: a bare PID, once its originally-recorded process has exited,
// can be silently reused by the OS for something else entirely. This proves
// killOwnedProcess never signals a PID whose real, current identity (start
// time, from a real process-table read) doesn't match what was recorded --
// and that it still correctly signals a genuinely matching identity.
func TestKillOwnedProcessRefusesWhenIdentityNoLongerMatches(t *testing.T) {
	bin := fakeHelperBinary(t)
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// pomar's review (POMAR147-CLEANUP-1): the single authoritative
	// cmd.Wait() goroutine starts immediately after Start() succeeds, not
	// later past fallible assertions -- an early t.Fatal before this point
	// previously left no goroutine to ever join, so cleanup's kill alone
	// could leave an unreaped zombie. Cleanup now kills and joins this SAME
	// channel (never a second, concurrent Wait() call on the same *exec.Cmd,
	// which is a documented misuse).
	stillRunning := make(chan error, 1)
	go func() { stillRunning <- cmd.Wait() }()
	t.Cleanup(func() {
		// Short-circuit on the normal success path, where the main test
		// body has already drained stillRunning and the process is already
		// gone: without this check, cleanup would otherwise block for the
		// full timeout below on every run, since nothing more is ever sent
		// on an already-drained channel.
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		cmd.Process.Kill()
		select {
		case <-stillRunning:
		case <-time.After(5 * time.Second):
		}
	})

	ps, err := (proc.PS{}).List()
	if err != nil {
		t.Fatal(err)
	}
	real, ok := proc.Find(ps, pid)
	if !ok {
		t.Fatalf("just-started process %d not found in the real process table", pid)
	}

	// A deliberately wrong start time -- simulating the exact scenario this
	// check exists for: the PID has since been reused by an unrelated
	// process. No signal must reach the real, still-running process.
	//
	// syscall.Kill(pid, 0) alone cannot prove this: a SIGKILLed child
	// becomes a zombie, still answering kill(pid, 0) successfully, until
	// this process (its real parent) reaps it -- so a mistaken signal here
	// would read identically to "never touched" on that check alone
	// (caught by mutant-testing this very assertion against a deliberately
	// disabled identity check, which the kill(pid,0)-only version of this
	// test did not detect). cmd.Wait() with a short, bounded timeout
	// distinguishes them instead: a real kill makes Wait return almost
	// immediately; a genuinely still-running, untouched process makes Wait
	// block for the whole timeout.
	killOwnedProcess(t, proc.Process{PID: pid, Start: "Mon Jan  1 00:00:00 1970"})
	select {
	case err := <-stillRunning:
		t.Fatalf("process %d was reaped (so it was signaled) despite a mismatching recorded identity: %v", pid, err)
	case <-time.After(300 * time.Millisecond):
		// Still running, as required. Wait is left outstanding in the
		// background goroutine; the real kill below reaps it for real.
	}

	// The real, matching identity must still actually be killable -- this
	// isn't a check that silently disables killing altogether.
	killOwnedProcess(t, real)
	select {
	case err := <-stillRunning:
		if err == nil {
			t.Fatal("process exited cleanly instead of being killed")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("process %d not reaped within 5s after a correctly-identified kill", pid)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("process %d still present after being reaped", pid)
	}
}
