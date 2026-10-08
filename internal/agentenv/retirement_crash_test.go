package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Restore the actual pre-unlink journal into a newly locked owner, rather than
// relying on observations or mutable pointers surviving in the original host.
func TestRetirementCrashBeforeUnlinkRechecksCustodyAndCapacity(t *testing.T) {
	for _, scenario := range []string{"same disk", "replacement disk", "new writer", "unsafe directory", "changed proof"} {
		t.Run(scenario, func(t *testing.T) {
			h, e, r := retirementFixture(t)
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "env")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			e.Spec.Directory, e.Spec.Rootfs = dir, filepath.Join(dir, "workspace.ext4")
			if err := os.WriteFile(e.Spec.Rootfs, []byte("retained original"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := HostConfig{Root: root, Helper: "/retained/helper", MaxLive: 1, CPUs: 1, MemoryBytes: 512 << 20}
			e.Spec.HostConfig = cfg
			s, n, err := privateRetirementDisk(e)
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(r)
			ret := &Retirement{Request: r, InputSHA256: digest(input), WorkspaceState: "cleanup_pending", Device: uint64(s.Dev), Inode: s.Ino, Bytes: n}
			probe := func(_ context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				stat, _, err := privateRetirementDisk(e)
				if err != nil {
					return FenceProbeEvidence{}, err
				}
				return FenceProbeEvidence{Environment: p.Environment, Session: p.Session, Incarnation: p.Incarnation, Operation: p.Operation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Coverage: "direct-vz-private-disk/v1", ObserverUID: os.Getuid(), ProcessCount: 10, OwnProcessesChecked: 2, RootfsDevice: uint32(stat.Dev), RootfsInode: stat.Ino, Confirmed: true}, nil
			}
			h.probe = probe
			if err := h.retirementProbe(e, ret); err != nil {
				t.Fatal(err)
			}
			originalProof := filepath.Join(dir, ret.EvidenceFile)
			originalBytes, err := os.ReadFile(originalProof)
			if err != nil {
				t.Fatal(err)
			}
			e.Phase = "retired"
			e.Actions[r.OperationID] = Action{Kind: "retire", Expected: r.Incarnation, State: "cleanup_pending", Fence: e.LastFence, Retirement: ret}
			if err := h.save(e); err != nil {
				t.Fatal(err)
			}
			// The process disappears here: the disk has not been unlinked yet.
			owner, err := OpenHost(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			restored := owner.envs["env"]
			if restored.Phase != "retired" || restored.Actions[r.OperationID].State != "cleanup_pending" {
				t.Fatal("lost tombstone")
			}
			probes := 0
			owner.probe = func(c context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				probes++
				if scenario == "new writer" {
					return FenceProbeEvidence{}, errors.New("fresh observer found an unresolved disk writer")
				}
				return probe(c, p)
			}
			switch scenario {
			case "replacement disk":
				if err := os.Rename(e.Spec.Rootfs, e.Spec.Rootfs+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(e.Spec.Rootfs, []byte("replacement must survive"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsafe directory":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "changed proof":
				if err := os.WriteFile(originalProof, []byte("modified proof"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r.ResumeCleanup = true
			w := retireCall(owner, r)
			if scenario == "same disk" {
				if w.Code != 200 || probes != 1 {
					t.Fatal("missing fresh custody observation", w.Code, probes, w.Body.String())
				}
				if _, err := os.Lstat(e.Spec.Rootfs); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("original disk remains", err)
				}
				got, err := os.ReadFile(originalProof)
				if err != nil || string(got) != string(originalBytes) {
					t.Fatal("historical proof rewritten")
				}
				if got := restored.Actions[r.OperationID]; got.State != "completed" || got.Retirement.WorkspaceState != "absent_after_cleanup" {
					t.Fatal("cleanup outcome lost")
				}
			} else {
				if w.Code != 409 {
					t.Fatal("unsafe crash continuation accepted", w.Body.String())
				}
				if _, err := os.Lstat(e.Spec.Rootfs); err != nil {
					t.Fatal("refusal deleted disk", err)
				}
				if restored.Actions[r.OperationID].State != "cleanup_pending" {
					t.Fatal("refusal completed cleanup")
				}
			}
			// Retirement releases only this scope. Repeated completion/refusal must
			// never make room around another held scope at the one-slot limit.
			owner.envs["occupied"] = &Environment{Phase: "execution_unknown"}
			candidate := &Environment{Phase: "created"}
			for i := 0; i < 2; i++ {
				if scenario == "same disk" {
					if w := retireCall(owner, r); w.Code != 200 {
						t.Fatal(w.Body.String())
					}
				}
				if err := owner.start(candidate); err == nil || !strings.Contains(err.Error(), "environment limit reached") {
					t.Fatal("retirement released capacity twice", err)
				}
			}
		})
	}
}
