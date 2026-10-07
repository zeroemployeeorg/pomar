package manager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/result"
)

// The job environment the helper gives a command with a source
// (host/Sources/PomarHostCore/Helper.swift: jobUID, jobHome, proxyEnvironment).
// The result records what Pomar set, never the image's own environment.
const (
	jobUID  = 1000
	jobHome = "/pomar/job"
	// jobHomeWithoutSource is HOME for a job-user start with no source.
	jobHomeWithoutSource = "/tmp"
	jobGoProxy           = "http://127.0.0.1:7070"
)

// resultDoc is the result document of the CI record design: what an attempt
// ran, pinned at admission, and how it ended. Canonical JSON sorts its keys.
func (m *Manager) resultDoc(e Entry) map[string]any {
	doc := map[string]any{
		"schema":         result.Schema,
		"attempt":        e.Attempt,
		"command":        e.Command,
		"state":          string(e.State),
		"exit_code":      e.ExitCode, // null when the attempt had none
		"host_condition": e.HostCondition,
		"reason":         e.Reason,
		"created":        e.Created.UTC().Format(time.RFC3339Nano),
		"ended":          e.Ended.UTC().Format(time.RFC3339Nano),
		"class":          e.Class,
		"peaks":          e.Peaks, // informational; not part of any decision
		"host":           "",
	}
	if m.cfg.Signer != nil {
		doc["host"] = m.cfg.Signer.ID // the key's fingerprint, never a host name
	}
	if p := e.Pins; p != nil {
		doc["pomar_version"] = p.Pomar
		doc["command_sha256"] = p.CommandHash
		doc["image_digest"] = p.Image
		doc["image_arm64"] = p.ImageArm64
		doc["package_set"] = p.PackageSet
		doc["kernel_sha256"] = p.Kernel
		doc["vminit_digest"] = p.Init
		doc["host_bin_sha256"] = p.HostBin
		if p.RootfsSHA256 != "" {
			doc["rootfs_sha256"] = p.RootfsSHA256
		}
	}
	env := map[string]any{}
	if s := e.Source; s != nil {
		doc["mirror"] = s.Mirror
		doc["ref"] = s.Ref
		doc["sha"] = s.SHA
		doc["git"] = s.Git
		doc["release_base"] = s.Base
		if s.BaseSHA != "" {
			doc["base_sha"] = s.BaseSHA
		}
		if s.Reach != nil {
			// How the commits were reachable at the sync that admitted it.
			doc["reach"] = s.Reach
		}
		if m.cfg.Mirrors != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if url, err := m.cfg.Mirrors.Remote(ctx, s.Mirror); err == nil {
				doc["repo"] = url
			}
			cancel()
		}
		env["uid"], env["gid"], env["HOME"] = jobUID, jobUID, jobHome
		if e.GoProxy {
			env["GOPROXY"] = jobGoProxy
		}
		if s.ReadOnly {
			doc["readonly_source"] = true
		}
	} else if e.JobUser {
		env["uid"], env["gid"], env["HOME"] = jobUID, jobUID, jobHomeWithoutSource
	}
	doc["env"] = env
	if e.StartedByUID != nil {
		doc["started_by_uid"] = *e.StartedByUID
	}
	if e.NPM != nil {
		doc["npm_lock"] = e.NPM
	}
	if len(e.Inputs) > 0 {
		// Bind the bytes validated at admission, without including their
		// contents or rereading mutable files at completion.
		doc["inputs"] = e.Inputs
	}
	if e.Log != nil {
		doc["output_log"] = e.Log
	}
	if len(e.OutputNames) > 0 {
		doc["outputs"] = e.Outputs
	}
	return doc
}

// writeResult writes a terminal attempt's result document once, and signs it
// when the manager has a key. A failure is an event, not a crash: the entry in
// the table stays the record of the attempt.
func (m *Manager) writeResult(e Entry) {
	dir := filepath.Join(m.cfg.Venue.Root(), attemptsDir, e.Attempt)
	doc, err := result.Canonical(m.resultDoc(e))
	if err == nil {
		var sig []byte
		if m.cfg.Signer != nil {
			sig = m.cfg.Signer.Sign(doc)
		}
		err = result.Write(dir, doc, sig)
	}
	if err != nil {
		m.event("result-error", e.Attempt, e.PID, err.Error())
		return
	}
	m.event("result-written", e.Attempt, e.PID, map[bool]string{true: "signed", false: "unsigned"}[m.cfg.Signer != nil])
}

// ResultReply is the body of GET /v1/attempts/{id}/result: the document as
// written (verify these exact bytes), its signature, and the key's id.
type ResultReply struct {
	Result    json.RawMessage `json:"result"`
	Signature string          `json:"signature,omitempty"` // base64 ed25519; empty when unsigned
	KeyID     string          `json:"key_id,omitempty"`
}

// KeyReply is the body of GET /v1/signing-key: the public key to pin.
type KeyReply struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"` // base64
}

func (m *Manager) resultRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/attempts/{id}/result", m.attemptGuard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid attempt id"})
			return
		}
		doc, sig, err := result.Read(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id))
		if errors.Is(err, fs.ErrNotExist) {
			reply(w, http.StatusNotFound, map[string]string{"error": "no result for " + id})
			return
		}
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out := ResultReply{Result: doc}
		if sig != nil {
			out.Signature = base64.StdEncoding.EncodeToString(sig)
			if m.cfg.Signer != nil {
				out.KeyID = m.cfg.Signer.ID
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out.exact())
	}))
	mux.HandleFunc("GET /v1/signing-key", func(w http.ResponseWriter, r *http.Request) {
		if m.cfg.Signer == nil {
			reply(w, http.StatusNotFound, map[string]string{"error": "this manager does not sign results"})
			return
		}
		reply(w, http.StatusOK, KeyReply{Algorithm: "ed25519", KeyID: m.cfg.Signer.ID,
			PublicKey: base64.StdEncoding.EncodeToString(m.cfg.Signer.Public())})
	})
}

// exact is the reply with the result document written in exactly as it was
// signed (the elders' ruling of 2026-09-27 §2.1). An encoder would re-encode
// it: Go's escapes <, > and & by default, and the bytes a verifier received
// would then differ from the bytes the manager signed.
func (r ResultReply) exact() []byte {
	var b bytes.Buffer
	b.WriteString(`{"result":`)
	b.Write(r.Result)
	if r.Signature != "" { // base64: nothing in it needs escaping
		b.WriteString(`,"signature":"` + r.Signature + `"`)
	}
	if r.KeyID != "" { // hex
		b.WriteString(`,"key_id":"` + r.KeyID + `"`)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// Result fetches an attempt's result reply as the exact bytes served, so the
// signed document inside it can be verified as it arrived.
func (c *Client) Result(id string) ([]byte, error) {
	return c.raw("/v1/attempts/" + id + "/result")
}
