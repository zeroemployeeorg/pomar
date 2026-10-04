package agentenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type ReconcileRequest struct {
	OperationID     string `json:"operation_id"`
	SessionID       string `json:"session_id"`
	Incarnation     string `json:"expected_incarnation"`
	LaunchOperation string `json:"launch_operation"`
	FenceOperation  string `json:"fence_operation"`
}

// Reconciliation is a new observation of current fencing; it never repairs a
// historical vm_stopped=false receipt or claims historical non-execution.
type Reconciliation struct {
	Request              ReconcileRequest `json:"request"`
	InputSHA256          string           `json:"input_sha256"`
	HistoricalExecution  string           `json:"historical_execution"`
	ObservedAt           string           `json:"observed_at"`
	CurrentScope         string           `json:"current_scope"`
	ReplacementEligible  bool             `json:"replacement_eligible"`
	EvidenceFile         string           `json:"evidence_file"`
	EvidenceSHA256       string           `json:"evidence_sha256"`
	OriginalConfigSHA256 string           `json:"original_config_sha256"`
	OriginalStatusSHA256 string           `json:"original_status_sha256"`
	Issues               []string         `json:"issues"`
}
type FenceProbeRequest struct {
	Environment string `json:"environment"`
	Session     string `json:"session"`
	Incarnation string `json:"incarnation"`
	Operation   string `json:"operation"`
	Rootfs      string `json:"rootfs"`
	Helper      string `json:"helper"`
	PID         int    `json:"pid"`
	UID         int    `json:"uid"`
	Birth       uint64 `json:"birth"`
}
type FenceProbeEvidence struct {
	Environment             string   `json:"environment"`
	Session                 string   `json:"session"`
	Incarnation             string   `json:"incarnation"`
	Operation               string   `json:"operation"`
	ObservedAt              string   `json:"observedAt"`
	Coverage                string   `json:"coverage"`
	ObserverUID             int      `json:"observerUID"`
	ProcessCount            int      `json:"processCount"`
	OwnProcessesChecked     int      `json:"ownProcessesChecked"`
	VnodeDescriptorsChecked int      `json:"vnodeDescriptorsChecked"`
	FileportsChecked        int      `json:"fileportsChecked"`
	RegionsChecked          int      `json:"regionsChecked"`
	KernelZombiesChecked    []int    `json:"kernelZombiesChecked"`
	RootfsDevice            uint32   `json:"rootfsDevice"`
	RootfsInode             uint64   `json:"rootfsInode"`
	Issues                  []string `json:"issues"`
	Confirmed               bool     `json:"confirmed"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func readVMStatus(e *Environment) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(e.Spec.Directory, "vm-status-"+e.Spec.Incarnation+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return os.ReadFile(filepath.Join(e.Spec.Directory, "vm-status.json"))
	}
	return b, err
}

// writeEvidence creates immutable private bytes and syncs the directory before
// the environment journal can reference them as replacement evidence.
func writeEvidence(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (h *Host) machineProbe(ctx context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
	if h.probe != nil {
		return h.probe(ctx, r)
	}
	var evidence FenceProbeEvidence
	b, _ := json.Marshal(r)
	file := filepath.Join(h.config.Root, r.Environment, "reconcile-probe-"+r.Operation+".json")
	if err := writeEvidence(file, b); err != nil {
		return evidence, err
	}
	// Only the service owner's native Swift helper supplies machine evidence.
	// The client cannot supply a receipt, arbitrary probe, or a termination claim.
	cmd := exec.CommandContext(ctx, h.config.Helper, "agent-environment-fence-probe", "--config", file)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + h.config.Root}
	output, err := cmd.Output()
	if err != nil {
		return evidence, fmt.Errorf("native executor/access probe failed: %w", err)
	}
	if len(output) > 256<<10 {
		return evidence, errors.New("native probe evidence exceeds limit")
	}
	if err = json.Unmarshal(output, &evidence); err != nil {
		return evidence, errors.New("native probe returned incomplete evidence")
	}
	return evidence, nil
}
func (h *Host) reconcileHandler(w http.ResponseWriter, r *http.Request) {
	var req ReconcileRequest
	if !decode(w, r, &req) {
		return
	}
	for _, id := range []string{req.OperationID, req.SessionID, req.Incarnation, req.LaunchOperation, req.FenceOperation} {
		if !validID.MatchString(id) {
			respond(w, 400, map[string]string{"error": "invalid reconciliation identity"})
			return
		}
	}
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
	input, _ := json.Marshal(req)
	hash := digest(input)
	if a, ok := e.Actions[req.OperationID]; ok {
		if a.Kind != "reconcile" || a.Reconciliation == nil || a.Reconciliation.InputSHA256 != hash {
			failure(w, ErrConflict)
			return
		}
		respond(w, 200, map[string]any{"operation_id": req.OperationID, "action": a})
		return
	}
	if req.Incarnation != e.Spec.Incarnation || req.SessionID != e.Spec.Session {
		failure(w, ErrStale)
		return
	}
	if h.fatal != nil {
		failure(w, errors.New("owner journal durability uncertain"))
		return
	}
	if e.Phase != "execution_unknown" && e.Phase != "revoking" && e.Phase != "reconciling" {
		failure(w, errors.New("reconciliation requires a held uncertain execution scope"))
		return
	}
	launch, launchOK := e.Actions[req.LaunchOperation]
	oldFence, fenceOK := e.Actions[req.FenceOperation]
	if !launchOK || (launch.Kind != "start" && launch.Kind != "replace") || launch.State != "completed" || !fenceOK || oldFence.Fence == nil || oldFence.Expected != req.Incarnation || oldFence.Fence.Incarnation != req.Incarnation || !oldFence.Fence.NetworkRevoked || !oldFence.Fence.AdapterAccessRevoked {
		failure(w, errors.New("original launch and retained scope revocation do not bind"))
		return
	}
	if launch.ResultingIncarnation != "" && launch.ResultingIncarnation != req.Incarnation {
		failure(w, ErrConflict)
		return
	}
	if launch.ResultingIncarnation == "" {
		// Legacy journals did not store the output incarnation on a start action.
		// Accept only an unambiguous sole completed launch, then bind the retained
		// immutable VM config and exact process command to the current incarnation.
		count := 0
		for _, a := range e.Actions {
			if (a.Kind == "start" || a.Kind == "replace") && a.State == "completed" {
				count++
			}
		}
		if count != 1 {
			failure(w, errors.New("legacy launch ownership is ambiguous"))
			return
		}
	}
	configFile := filepath.Join(e.Spec.Directory, "vm-config-"+req.Incarnation+".json")
	configBytes, err := os.ReadFile(configFile)
	if err != nil {
		failure(w, errors.New("original launch config unavailable"))
		return
	}
	var original VMSpec
	if json.Unmarshal(configBytes, &original) != nil || original.Environment != e.Spec.Environment || original.Session != req.SessionID || original.Incarnation != req.Incarnation || original.Rootfs != e.Spec.Rootfs || original.Helper != e.Spec.Helper {
		failure(w, errors.New("original launch config identity mismatch"))
		return
	}
	args := strings.Fields(e.Process.Args)
	if e.Process.UID != os.Getuid() || len(args) != 4 || args[0] != original.Helper || args[1] != "agent-environment" || args[2] != "--config" || args[3] != configFile {
		failure(w, errors.New("retained process launch ownership mismatch"))
		return
	}
	birth, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", e.Process.Start, time.Local)
	if err != nil || birth.Unix() <= 0 {
		failure(w, errors.New("retained process birth unavailable"))
		return
	}
	statusBytes, err := readVMStatus(e)
	if err != nil {
		failure(w, errors.New("original VM receipt unavailable"))
		return
	}
	var originalStatus map[string]string
	if json.Unmarshal(statusBytes, &originalStatus) != nil || originalStatus["incarnation"] != req.Incarnation || originalStatus["session"] != req.SessionID || originalStatus["environment"] != e.Spec.Environment {
		failure(w, errors.New("original VM receipt identity mismatch"))
		return
	}
	receipt := &Reconciliation{Request: req, InputSHA256: hash, HistoricalExecution: "unknown", CurrentScope: "unknown", OriginalConfigSHA256: digest(configBytes), OriginalStatusSHA256: digest(statusBytes), Issues: []string{}}
	e.Phase = "reconciling"
	e.LaunchRevoked = append(e.LaunchRevoked, req.Incarnation)
	e.Actions[req.OperationID] = Action{Kind: "reconcile", Expected: req.Incarnation, State: "dispatching", Reconciliation: receipt}
	if g := h.gates[e.Spec.Environment]; g != nil {
		g.Revoke()
	}
	for _, key := range []string{e.Spec.Environment, e.Spec.Environment + "-go"} {
		if s := h.servers[key]; s != nil {
			s.Close()
		}
	}
	if err = h.save(e); err != nil {
		failure(w, err)
		return
	}
	// The API guard, root ownership lock and operation lock are held through
	// the probe AND both durable commits. No replacement or forwarded adapter
	// request can acquire launch authority during this observation interval.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	evidence, probeErr := h.machineProbe(ctx, FenceProbeRequest{Environment: e.Spec.Environment, Session: req.SessionID, Incarnation: req.Incarnation, Operation: req.OperationID, Rootfs: e.Spec.Rootfs, Helper: original.Helper, PID: e.Process.PID, UID: e.Process.UID, Birth: uint64(birth.Unix())})
	if probeErr != nil {
		receipt.Issues = append(receipt.Issues, probeErr.Error())
	}
	receipt.Issues = append(receipt.Issues, evidence.Issues...)
	stat, statErr := os.Lstat(e.Spec.Rootfs)
	var disk *syscall.Stat_t
	if statErr == nil {
		disk, _ = stat.Sys().(*syscall.Stat_t)
	}
	if probeErr == nil && (evidence.Environment != e.Spec.Environment || evidence.Session != req.SessionID || evidence.Incarnation != req.Incarnation || evidence.Operation != req.OperationID || evidence.Coverage != "direct-vz-private-disk/v1" || evidence.ObserverUID != os.Getuid() || evidence.ProcessCount == 0 || evidence.OwnProcessesChecked == 0 || evidence.ObservedAt == "" || disk == nil || uint32(disk.Dev) != evidence.RootfsDevice || disk.Ino != evidence.RootfsInode) {
		receipt.Issues = append(receipt.Issues, "native probe identity or coverage incomplete")
	}
	receipt.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(evidence)
	receipt.EvidenceFile = "reconcile-evidence-" + req.OperationID + ".json"
	receipt.EvidenceSHA256 = digest(b)
	if err = writeEvidence(filepath.Join(e.Spec.Directory, receipt.EvidenceFile), b); err != nil {
		h.fatal = err
		failure(w, err)
		return
	}
	confirmed := probeErr == nil && evidence.Confirmed && len(receipt.Issues) == 0
	fence := &Fence{Incarnation: req.Incarnation, ScopeID: "scope-" + req.Incarnation, NetworkRevoked: true, AdapterAccessRevoked: true, WorkspaceWriteRevoked: confirmed, TerminationConfirmed: confirmed, State: "execution_unknown"}
	e.Phase = "execution_unknown"
	if confirmed {
		receipt.CurrentScope = "fenced_now"
		receipt.ReplacementEligible = true
		fence.State = "confirmed_current"
		e.Phase = "fenced"
	}
	a := e.Actions[req.OperationID]
	a.State = "completed"
	if !confirmed {
		a.State = "execution_unknown"
	}
	a.Fence = fence
	e.Actions[req.OperationID] = a
	e.LastFence = fence
	if err = h.save(e); err != nil {
		e.Phase = "execution_unknown"
		receipt.ReplacementEligible = false
		h.fatal = err
		failure(w, err)
		return
	}
	// The old stop Action.Fence and vm-status bytes were never mutated.
	respond(w, 200, map[string]any{"operation_id": req.OperationID, "action": a})
}
