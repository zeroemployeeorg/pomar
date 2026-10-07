package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Retirement is an owner decision separate from stopping compute. The disk
// contains both workspace and provider authentication; deletion requires an
// explicit choice and an owner-retained preservation/retention reference.
type RetireRequest struct {
	OperationID        string `json:"operation_id"`
	SessionID          string `json:"session_id"`
	Incarnation        string `json:"expected_incarnation"`
	FenceOperation     string `json:"fence_operation"`
	Mode               string `json:"mode"`
	RetentionReference string `json:"retention_reference"`
	ResumeCleanup      bool   `json:"resume_cleanup,omitempty"`
}

type Retirement struct {
	Request        RetireRequest `json:"request"`
	InputSHA256    string        `json:"input_sha256"`
	WorkspaceState string        `json:"workspace_state"`
	Device         uint64        `json:"device,omitempty"`
	Inode          uint64        `json:"inode,omitempty"`
	Bytes          int64         `json:"logical_bytes,omitempty"`
	EvidenceFile   string        `json:"evidence_file,omitempty"`
	EvidenceSHA256 string        `json:"evidence_sha256,omitempty"`
}

func retiredFence(e *Environment, r RetireRequest) bool {
	a, ok := e.Actions[r.FenceOperation]
	f := a.Fence
	return ok && a.State == "completed" && (a.Kind == "stop" || a.Kind == "reconcile") &&
		a.Expected == r.Incarnation && f != nil && e.LastFence != nil && *f == *e.LastFence &&
		f.Incarnation == r.Incarnation && f.NetworkRevoked && f.AdapterAccessRevoked &&
		f.WorkspaceWriteRevoked && f.TerminationConfirmed
}

func privateRetirementDisk(e *Environment) (*syscall.Stat_t, int64, error) {
	dir, err := os.Lstat(e.Spec.Directory)
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, 0, errors.New("retirement directory custody unavailable")
	}
	d, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || int(d.Uid) != os.Getuid() || e.Spec.Rootfs != filepath.Join(e.Spec.Directory, "workspace.ext4") {
		return nil, 0, errors.New("retirement disk binding unavailable")
	}
	fi, err := os.Lstat(e.Spec.Rootfs)
	if err != nil {
		return nil, 0, err
	}
	s, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 || int(s.Uid) != os.Getuid() || s.Nlink != 1 {
		return nil, 0, errors.New("retirement requires an exclusively named private regular disk")
	}
	return s, fi.Size(), nil
}

func (h *Host) retireHandler(w http.ResponseWriter, r *http.Request) {
	var req RetireRequest
	if !decode(w, r, &req) {
		return
	}
	for _, id := range []string{req.OperationID, req.SessionID, req.Incarnation, req.FenceOperation} {
		if !validID.MatchString(id) {
			failure(w, errors.New("invalid retirement identity"))
			return
		}
	}
	if (req.Mode != "retain" && req.Mode != "delete-workspace-and-auth") || req.RetentionReference == "" || len(req.RetentionReference) > 1024 {
		failure(w, errors.New("explicit retention mode and owner retention reference required"))
		return
	}
	resume := req.ResumeCleanup
	req.ResumeCleanup = false // The continuation does not change the original intent.
	b, _ := json.Marshal(req)
	hash := digest(b)
	h.mu.Lock()
	e := h.envs[r.PathValue("id")]
	h.mu.Unlock()
	if e == nil {
		respond(w, 404, map[string]string{"error": "unknown environment"})
		return
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fatal != nil {
		failure(w, errors.New("owner journal durability uncertain"))
		return
	}
	if req.Incarnation != e.Spec.Incarnation || req.SessionID != e.Spec.Session {
		failure(w, ErrStale)
		return
	}
	a, exists := e.Actions[req.OperationID]
	if exists {
		if a.Kind != "retire" || a.Retirement == nil || a.Retirement.InputSHA256 != hash {
			failure(w, ErrConflict)
			return
		}
		if a.State == "completed" {
			respond(w, 200, a)
			return
		}
		if !resume || a.State != "cleanup_pending" || e.Phase != "retired" || req.Mode != "delete-workspace-and-auth" {
			failure(w, errors.New("inspect retained retirement; explicit cleanup continuation required"))
			return
		}
		if err := validateRetirementProof(e, a.Retirement); err != nil {
			failure(w, err)
			return
		}
	} else {
		if resume || (e.Phase != "stopped" && e.Phase != "fenced") {
			failure(w, errors.New("retirement requires a confirmed stopped scope"))
			return
		}
	}
	if !retiredFence(e, req) {
		failure(w, errors.New("retirement requires the original complete current fence"))
		return
	}
	if !exists {
		ret := &Retirement{Request: req, InputSHA256: hash, WorkspaceState: "retained"}
		if req.Mode == "delete-workspace-and-auth" {
			s, size, err := privateRetirementDisk(e)
			if err != nil {
				failure(w, err)
				return
			}
			ret.Device, ret.Inode, ret.Bytes = uint64(s.Dev), s.Ino, size
			if err = h.retirementProbe(e, ret); err != nil {
				failure(w, err)
				return
			}
			ret.WorkspaceState = "cleanup_pending"
		}
		a = Action{Kind: "retire", Expected: req.Incarnation, State: "completed", Fence: e.LastFence, Retirement: ret}
		if req.Mode == "delete-workspace-and-auth" {
			a.State = "cleanup_pending"
		}
		e.Actions[req.OperationID] = a
		// Persist the terminal compute/launch tombstone BEFORE any unlink. A
		// crash cannot restore launch authority around partially removed state.
		e.Phase = "retired"
		if err := h.save(e); err != nil {
			failure(w, err)
			return
		}
	}
	if req.Mode == "delete-workspace-and-auth" {
		ret := a.Retirement
		s, _, err := privateRetirementDisk(e)
		if err == nil {
			if uint64(s.Dev) != ret.Device || s.Ino != ret.Inode {
				failure(w, errors.New("retirement disk identity changed"))
				return
			}
			if exists {
				if err = h.retirementProbe(e, ret); err != nil {
					failure(w, err)
					return
				}
			}
			if err = os.Remove(e.Spec.Rootfs); err != nil {
				failure(w, err)
				return
			}
		} else if !exists || !errors.Is(err, os.ErrNotExist) {
			failure(w, err)
			return
		}
		// Absence completes only this previously fenced, durably tombstoned
		// cleanup. It is never evidence to clear an uncertain execution scope.
		dir, err := os.Open(e.Spec.Directory)
		if err == nil {
			err = dir.Sync()
			dir.Close()
		}
		if err != nil {
			failure(w, err)
			return
		}
		ret.WorkspaceState = "absent_after_cleanup"
		a.State = "completed"
		e.Actions[req.OperationID] = a
		if err = h.save(e); err != nil {
			failure(w, err)
			return
		}
	}
	respond(w, 200, a)
}

func validateRetirementProof(e *Environment, ret *Retirement) error {
	if filepath.Base(ret.EvidenceFile) != ret.EvidenceFile || !strings.HasPrefix(ret.EvidenceFile, "retirement-evidence-"+ret.Request.OperationID+"-") || ret.Device == 0 || ret.Inode == 0 {
		return errors.New("retirement cleanup proof binding unavailable")
	}
	b, err := os.ReadFile(filepath.Join(e.Spec.Directory, ret.EvidenceFile))
	var v FenceProbeEvidence
	if err != nil || digest(b) != ret.EvidenceSHA256 || json.Unmarshal(b, &v) != nil || !v.Confirmed || len(v.Issues) != 0 || v.Environment != e.Spec.Environment || v.Session != e.Spec.Session || v.Incarnation != e.Spec.Incarnation || !strings.HasPrefix(v.Operation, ret.Request.OperationID+"-") || v.Coverage != "direct-vz-private-disk/v1" || v.ObserverUID != os.Getuid() || v.ProcessCount == 0 || v.OwnProcessesChecked == 0 || v.ObservedAt == "" || uint64(v.RootfsDevice) != uint64(uint32(ret.Device)) || v.RootfsInode != ret.Inode {
		return errors.New("retirement cleanup proof incomplete or changed")
	}
	return nil
}

func (h *Host) retirementProbe(e *Environment, ret *Retirement) error {
	birth, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", e.Process.Start, time.Local)
	if err != nil || birth.Unix() <= 0 || e.Process.PID <= 0 || e.Process.UID != os.Getuid() {
		return errors.New("retirement process binding incomplete")
	}
	// A continuation gets a separate immutable observation, without rewriting
	// the first probe or historical lifecycle receipts.
	op := ret.Request.OperationID + "-" + randomID()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	v, err := h.machineProbe(ctx, FenceProbeRequest{Environment: e.Spec.Environment, Session: e.Spec.Session, Incarnation: e.Spec.Incarnation, Operation: op, Rootfs: e.Spec.Rootfs, Helper: e.Spec.Helper, PID: e.Process.PID, UID: e.Process.UID, Birth: uint64(birth.Unix())})
	if err != nil {
		return err
	}
	s, _, err := privateRetirementDisk(e)
	if err != nil {
		return err
	}
	if !v.Confirmed || len(v.Issues) != 0 || v.Environment != e.Spec.Environment || v.Session != e.Spec.Session || v.Incarnation != e.Spec.Incarnation || v.Operation != op || v.ObserverUID != os.Getuid() || v.Coverage != "direct-vz-private-disk/v1" || v.ProcessCount == 0 || v.OwnProcessesChecked == 0 || v.ObservedAt == "" || uint64(s.Dev) != ret.Device || s.Ino != ret.Inode || v.RootfsDevice != uint32(s.Dev) || v.RootfsInode != s.Ino {
		return errors.New("retirement native executor/writer coverage incomplete")
	}
	b, _ := json.Marshal(v)
	file := "retirement-evidence-" + op + ".json"
	if err = writeEvidence(filepath.Join(e.Spec.Directory, file), b); err != nil {
		h.fatal = err
		return err
	}
	ret.EvidenceFile, ret.EvidenceSHA256 = file, digest(b)
	return nil
}
