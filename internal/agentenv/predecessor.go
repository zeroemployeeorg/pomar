package agentenv

import (
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// predecessorOperation accepts only the authoritative current observation.
// Never order operation IDs or timestamps to infer which observation is newest.
func predecessorOperation(e *Environment, incarnation string) string {
	id := e.CurrentReconciliation
	a, ok := e.Actions[id]
	if !ok || id == "" {
		return ""
	}
	r := a.Reconciliation
	f := a.Fence
	if a.Kind == "reconcile" && a.State == "completed" && a.Expected == incarnation && r != nil && f != nil && f.Incarnation == incarnation && f.NetworkRevoked && f.AdapterAccessRevoked && f.WorkspaceWriteRevoked && f.TerminationConfirmed && r.Request.OperationID == id && r.Request.SessionID == e.Spec.Session && r.Request.Incarnation == incarnation && r.CurrentScope == "fenced_now" && r.ReplacementEligible && len(r.Issues) == 0 {
		return id
	}
	return ""
}

// ensurePredecessor runs before incarnation advance under the host and
// environment locks. It uses the same trusted probe and durable commits as
// explicit reconciliation; a failed observation never becomes launch authority.
func (h *Host) ensurePredecessor(e *Environment) (string, error) {
	if h.fatal != nil {
		return "", errors.New("owner journal durability uncertain")
	}
	if id := predecessorOperation(e, e.Spec.Incarnation); id != "" {
		r := e.Actions[id].Reconciliation
		// Do not turn an in-memory or damaged receipt into recovered authority.
		if filepath.Base(r.EvidenceFile) != r.EvidenceFile || r.EvidenceFile == "." || r.EvidenceFile == "" {
			return "", errors.New("predecessor evidence path invalid")
		}
		b, err := localclient.ReadPrivate(filepath.Join(e.Spec.Directory, r.EvidenceFile), 256<<10)
		if err != nil || digest(b) != r.EvidenceSHA256 {
			return "", errors.New("predecessor evidence unavailable or changed")
		}
		return id, nil
	}
	// An ordinary stop is not a native access probe. It must not restore a
	// superseded positive receipt, nor replay the deterministic automatic ID.
	// Unmarked legacy observations have no reliable ordering: require a new
	// explicit reconciliation instead of guessing from IDs or wall-clock time.
	if id := e.CurrentReconciliation; id != "" {
		a, ok := e.Actions[id]
		if !ok || a.Kind != "reconcile" || a.Expected == e.Spec.Incarnation {
			return "", errors.New("current predecessor reconciliation is not eligible; fresh explicit reconciliation required")
		}
	}
	for _, a := range e.Actions {
		if a.Kind == "reconcile" && a.Expected == e.Spec.Incarnation {
			return "", errors.New("predecessor observation is not current; fresh explicit reconciliation required")
		}
	}
	ids := make([]string, 0, len(e.Actions))
	for id := range e.Actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	launch, fence := "", ""
	legacy, launches := "", 0
	for _, id := range ids {
		a := e.Actions[id]
		if (a.Kind == "start" || a.Kind == "replace") && a.State == "completed" {
			launches++
			if a.ResultingIncarnation == e.Spec.Incarnation {
				launch = id
			}
			if a.ResultingIncarnation == "" {
				legacy = id
			}
		}
		if a.Expected == e.Spec.Incarnation && a.Fence != nil && a.Fence.Incarnation == e.Spec.Incarnation && a.Fence.NetworkRevoked && a.Fence.AdapterAccessRevoked {
			fence = id
		}
	}
	if launch == "" && launches == 1 {
		launch = legacy
	}
	if launch == "" || fence == "" {
		return "", errors.New("predecessor launch/revocation evidence unavailable; incarnation retained")
	}
	id := "predecessor-" + digest([]byte(e.Spec.Session+"\x00"+e.Spec.Incarnation))
	a, err := h.reconcileLocked(e, ReconcileRequest{OperationID: id, SessionID: e.Spec.Session, Incarnation: e.Spec.Incarnation, LaunchOperation: launch, FenceOperation: fence})
	if err != nil {
		return "", err
	}
	if a.State != "completed" || a.Reconciliation == nil || !a.Reconciliation.ReplacementEligible {
		return "", errors.New("predecessor probe incomplete; inspect retained reconciliation before launch")
	}
	return id, nil
}
