package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retirementFixture(t *testing.T) (*Host, *Environment, RetireRequest) {
	t.Helper()
	h, e, _, _ := reconciliationFixture(t)
	e.Phase = "stopped"
	e.LastFence = &Fence{Incarnation: "actor", ScopeID: "scope-actor", NetworkRevoked: true, AdapterAccessRevoked: true, WorkspaceWriteRevoked: true, TerminationConfirmed: true, State: "confirmed"}
	e.Actions["stop"] = Action{Kind: "stop", Expected: "actor", State: "completed", Fence: e.LastFence}
	return h, e, RetireRequest{OperationID: "retire", SessionID: "session", Incarnation: "actor", FenceOperation: "stop", Mode: "delete-workspace-and-auth", RetentionReference: "owner verified archive and selected deletion"}
}

func retireCall(h *Host, r RetireRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(r)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/retire", strings.NewReader(string(b))))
	return w
}

func TestRetirementDeletesOnlyExplicitDiskAndKeepsEvidence(t *testing.T) {
	h, e, r := retirementFixture(t)
	config, _ := os.ReadFile(filepath.Join(e.Spec.Directory, "vm-config-actor.json"))
	status, _ := os.ReadFile(filepath.Join(e.Spec.Directory, "vm-status.json"))
	old := e.Actions["stop"]
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a := e.Actions["retire"]
	if e.Phase != "retired" || a.State != "completed" || a.Retirement.WorkspaceState != "absent_after_cleanup" {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Lstat(e.Spec.Rootfs); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retained disk not deleted")
	}
	for name, want := range map[string][]byte{"vm-config-actor.json": config, "vm-status.json": status} {
		got, err := os.ReadFile(filepath.Join(e.Spec.Directory, name))
		if err != nil || string(got) != string(want) {
			t.Fatal("historical evidence altered")
		}
	}
	if e.Actions["stop"] != old {
		t.Fatal("stop evidence rewritten")
	}
	proof, err := os.ReadFile(filepath.Join(e.Spec.Directory, a.Retirement.EvidenceFile))
	if err != nil || digest(proof) != a.Retirement.EvidenceSHA256 {
		t.Fatal("native proof not retained")
	}
	h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
		t.Fatal("completed retry repeated probe")
		return FenceProbeEvidence{}, nil
	}
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r.Mode = "retain"
	if w := retireCall(h, r); w.Code != 409 {
		t.Fatal("operation id rebound")
	}
	if err := h.start(e); err == nil {
		t.Fatal("retired environment restarted")
	}
}

func TestRetirementRetainDecisionDoesNotDeleteGuestState(t *testing.T) {
	h, e, r := retirementFixture(t)
	r.Mode = "retain"
	h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
		t.Fatal("retention performed deletion probe")
		return FenceProbeEvidence{}, nil
	}
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	b, err := os.ReadFile(e.Spec.Rootfs)
	if err != nil || string(b) != "retained" || e.Phase != "retired" || e.Actions[r.OperationID].Retirement.WorkspaceState != "retained" {
		t.Fatal("retain mode destroyed state")
	}
	if err := h.start(e); err == nil {
		t.Fatal("retired retention restarted")
	}
}

func TestRetirementRefusesUncertainOrRedirectedOwnership(t *testing.T) {
	for _, mode := range []string{"running", "unknown", "incomplete fence", "stale", "no choice", "no retention", "symlink", "hardlink", "probe error", "possible VM", "probe identity", "inode substitution", "journal failure"} {
		t.Run(mode, func(t *testing.T) {
			h, e, r := retirementFixture(t)
			original := h.probe
			switch mode {
			case "running":
				e.Phase = "running"
			case "unknown":
				e.Phase = "execution_unknown"
			case "incomplete fence":
				e.LastFence.WorkspaceWriteRevoked = false
			case "stale":
				r.Incarnation = "other"
			case "no choice":
				r.Mode = ""
			case "no retention":
				r.RetentionReference = ""
			case "symlink":
				os.Rename(e.Spec.Rootfs, e.Spec.Rootfs+"-original")
				os.Symlink(e.Spec.Rootfs+"-original", e.Spec.Rootfs)
			case "hardlink":
				os.Link(e.Spec.Rootfs, e.Spec.Rootfs+"-other")
			case "journal failure":
				os.Mkdir(filepath.Join(e.Spec.Directory, "environment.json"), 0700)
			}
			h.probe = func(ctx context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				v, err := original(ctx, p)
				switch mode {
				case "probe error":
					return v, errors.New("inaccessible writer")
				case "possible VM":
					v.Issues = []string{"unresolved executor"}
				case "probe identity":
					v.Session = "wrong"
				case "inode substitution":
					os.Rename(e.Spec.Rootfs, e.Spec.Rootfs+"-original")
					os.WriteFile(e.Spec.Rootfs, []byte("replacement"), 0600)
				}
				return v, err
			}
			if w := retireCall(h, r); w.Code == 200 {
				t.Fatal("uncertain retirement succeeded")
			}
			if _, err := os.Lstat(e.Spec.Rootfs); err != nil {
				t.Fatal("refused operation deleted state")
			}
		})
	}
}

func TestRetirementCleanupContinuationAfterInterruptedUnlink(t *testing.T) {
	h, e, r := retirementFixture(t)
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// Model the durable pre-unlink journal restored after a crash following
	// unlink but preceding the final synced metadata commit.
	a := e.Actions[r.OperationID]
	a.State = "cleanup_pending"
	a.Retirement.WorkspaceState = "cleanup_pending"
	e.Actions[r.OperationID] = a
	if err := h.save(e); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restored Environment
	if err = json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	h.envs["env"] = &restored
	if w := retireCall(h, r); w.Code != 409 {
		t.Fatal("implicit cleanup retry accepted")
	}
	r.ResumeCleanup = true
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if restored.Phase != "retired" || restored.Actions[r.OperationID].Retirement.WorkspaceState != "absent_after_cleanup" {
		t.Fatal("cleanup not recovered")
	}
	if err = h.start(&restored); err == nil {
		t.Fatal("partial cleanup restored launch authority")
	}
}

func TestRetirementCleanupReprobesOriginalDiskAndRejectsReplacement(t *testing.T) {
	for _, substitute := range []bool{false, true} {
		t.Run(map[bool]string{false: "same disk", true: "replacement disk"}[substitute], func(t *testing.T) {
			h, e, r := retirementFixture(t)
			s, _, _ := privateRetirementDisk(e)
			input, _ := json.Marshal(r)
			e.Phase = "retired"
			e.Actions[r.OperationID] = Action{Kind: "retire", Expected: r.Incarnation, State: "cleanup_pending", Retirement: &Retirement{Request: r, InputSHA256: digest(input), Device: uint64(s.Dev), Inode: s.Ino, WorkspaceState: "cleanup_pending"}}
			if err := h.retirementProbe(e, e.Actions[r.OperationID].Retirement); err != nil {
				t.Fatal(err)
			}
			if substitute {
				os.Rename(e.Spec.Rootfs, e.Spec.Rootfs+"-original")
				os.WriteFile(e.Spec.Rootfs, []byte("different"), 0600)
			}
			calls := 0
			original := h.probe
			h.probe = func(c context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				calls++
				return original(c, p)
			}
			r.ResumeCleanup = true
			w := retireCall(h, r)
			if substitute {
				if w.Code != 409 {
					t.Fatal("replacement deleted")
				}
				if _, err := os.Lstat(e.Spec.Rootfs); err != nil {
					t.Fatal(err)
				}
			} else if w.Code != 200 || calls != 1 {
				t.Fatal(w.Body.String(), calls)
			}
		})
	}
}

func TestRetirementProbeNeedsPositiveDiskCoverage(t *testing.T) {
	h, e, r := retirementFixture(t)
	h.probe = func(_ context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
		s, _, _ := privateRetirementDisk(e)
		return FenceProbeEvidence{Environment: p.Environment, Session: p.Session, Incarnation: p.Incarnation, Operation: p.Operation, ObserverUID: os.Getuid(), ObservedAt: time.Now().Format(time.RFC3339), Coverage: "direct-vz-private-disk/v1", RootfsDevice: uint32(s.Dev), RootfsInode: s.Ino, Confirmed: true}, nil
	}
	if w := retireCall(h, r); w.Code != 409 {
		t.Fatal("empty inventory accepted")
	}
	if _, err := os.Lstat(e.Spec.Rootfs); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementTombstoneSurvivesOwnerRestart(t *testing.T) {
	h, e, r := retirementFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "env")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace.ext4"), []byte("unique guest data"), 0600); err != nil {
		t.Fatal(err)
	}
	e.Spec.Directory, e.Spec.Rootfs = dir, filepath.Join(dir, "workspace.ext4")
	h.config = HostConfig{Root: root, Helper: "/retained/helper", MaxLive: 1, CPUs: 1, MemoryBytes: 512 << 20}
	h.probe = func(_ context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
		s, _, err := privateRetirementDisk(e)
		if err != nil {
			return FenceProbeEvidence{}, err
		}
		return FenceProbeEvidence{Environment: p.Environment, Session: p.Session, Incarnation: p.Incarnation, Operation: p.Operation, ObserverUID: os.Getuid(), ObservedAt: time.Now().Format(time.RFC3339), Coverage: "direct-vz-private-disk/v1", RootfsDevice: uint32(s.Dev), RootfsInode: s.Ino, ProcessCount: 10, OwnProcessesChecked: 2, Confirmed: true}, nil
	}
	if w := retireCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	owner, err := OpenHost(h.config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.envs["env"].Phase != "retired" || owner.envs["env"].Actions[r.OperationID].Retirement.WorkspaceState != "absent_after_cleanup" {
		t.Fatal("retirement lost on restart")
	}
	if err = owner.start(owner.envs["env"]); err == nil {
		t.Fatal("restart restored launch authority")
	}
	if second, err := OpenHost(h.config); err == nil {
		second.Close()
		t.Fatal("second data-root writer admitted")
	}
	if w := retireCall(owner, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
