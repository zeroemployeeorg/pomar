// Package agentartifact receives binary candidates carried as bounded text by
// the development result endpoint. It does not publish or approve candidates.
package agentartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

const (
	ManifestPath   = "pomar-export/manifest.json"
	UntrackedLimit = 4 << 20
	EncodedLimit   = UntrackedLimit - (256 << 10)
	DecodedLimit   = EncodedLimit / 4 * 3
	ResultLimit    = 32 << 20
)

// Binding must come from the controller's retained task, not from the result.
type Binding struct {
	Environment string `json:"environment_id"`
	Session     string `json:"session_id"`
	Incarnation string `json:"incarnation"`
	Workspace   string `json:"workspace_id"`
	Scope       string `json:"scope_id"`
	Source      string `json:"source_sha"`
	Operation   string `json:"operation_id"`
	InputHash   string `json:"input_sha256"`
}

type Artifact struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	EncodingPath string `json:"encoding_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type Manifest struct {
	Version   string     `json:"version"`
	Artifacts []Artifact `json:"artifacts"`
}

type Receipt struct {
	Version   string     `json:"version"`
	Binding   Binding    `json:"binding"`
	Artifacts []Artifact `json:"artifacts"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,180}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var sourcePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func strictJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// Receive checks the completed task and all artifact bytes before creating a
// fresh private output directory. Existing directories are never overwritten.
func Receive(reader io.Reader, expected Binding, output string) (Receipt, error) {
	fail := func(reason string) (Receipt, error) { return Receipt{}, errors.New(reason) }
	if expected.Environment == "" || expected.Session == "" || expected.Incarnation == "" || expected.Workspace == "" || expected.Scope == "" || expected.Operation == "" || !shaPattern.MatchString(expected.InputHash) || !sourcePattern.MatchString(expected.Source) {
		return fail("complete independently retained binding required")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, ResultLimit+1))
	if err != nil {
		return Receipt{}, err
	}
	if len(raw) > ResultLimit {
		return fail("result exceeds receiver limit")
	}
	var result struct {
		Session   agentenv.State    `json:"session"`
		Diff      string            `json:"diff"`
		Untracked map[string]string `json:"untracked"`
	}
	if err := strictJSON(raw, &result); err != nil {
		return Receipt{}, err
	}
	s := result.Session
	if s.ContractVersion != "pomar.agent/v1" || s.EnvironmentID != expected.Environment || s.SessionID != expected.Session || s.Incarnation != expected.Incarnation || s.WorkspaceID != expected.Workspace || s.ScopeID != expected.Scope || s.SourceSHA != expected.Source {
		return fail("result identity does not match controller binding")
	}
	op, ok := s.Operations[expected.Operation]
	if !ok || op.ID != expected.Operation || op.Incarnation != expected.Incarnation || op.InputHash != expected.InputHash || op.State != "finished" || op.Completion != "completed" || !op.ActorAcknowledged {
		return fail("matching acknowledged completed task required")
	}
	if len(result.Diff) > UntrackedLimit || len(result.Untracked) > 32 {
		return fail("development export bounds exceeded")
	}
	total := 0
	for _, data := range result.Untracked {
		total += len(data)
		if total > UntrackedLimit {
			return fail("untracked export exceeds 4 MiB")
		}
	}
	manifestRaw, ok := result.Untracked[ManifestPath]
	if !ok || len(manifestRaw) > 16<<10 {
		return fail("bounded artifact manifest required")
	}
	var manifest Manifest
	if err := strictJSON([]byte(manifestRaw), &manifest); err != nil {
		return Receipt{}, err
	}
	if manifest.Version != "pomar.candidate/v1" || len(manifest.Artifacts) != 2 {
		return fail("one wheel and one sdist required")
	}
	seen := map[string]bool{}
	decoded := map[string][]byte{}
	encodedTotal := 0
	decodedTotal := 0
	for _, a := range manifest.Artifacts {
		if seen[a.Kind] || (a.Kind != "wheel" && a.Kind != "sdist") {
			return fail("duplicate or unsupported artifact kind")
		}
		seen[a.Kind] = true
		if !namePattern.MatchString(a.Name) || (a.Kind == "wheel" && !strings.HasSuffix(a.Name, ".whl")) || (a.Kind == "sdist" && !strings.HasSuffix(a.Name, ".tar.gz")) || a.EncodingPath != "pomar-export/"+a.Kind+".b64" || a.Size <= 0 || a.Size > DecodedLimit || !shaPattern.MatchString(a.SHA256) {
			return fail("invalid artifact name, encoding path, size or hash")
		}
		encoded, ok := result.Untracked[a.EncodingPath]
		if !ok || len(encoded) != base64.StdEncoding.EncodedLen(int(a.Size)) {
			return fail("missing or incorrectly sized encoded artifact")
		}
		encodedTotal += len(encoded)
		if encodedTotal > EncodedLimit {
			return fail("encoded artifacts exceed reserved export budget")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || base64.StdEncoding.EncodeToString(data) != encoded || int64(len(data)) != a.Size {
			return fail("artifact is not canonical base64 of the declared size")
		}
		decodedTotal += len(data)
		if decodedTotal > DecodedLimit {
			return fail("decoded artifacts exceed limit")
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != a.SHA256 {
			return fail("received artifact bytes fail SHA-256 verification")
		}
		decoded[a.Name] = data
	}
	receipt := Receipt{Version: "pomar.candidate-receipt/v1", Binding: expected, Artifacts: manifest.Artifacts}
	receiptJSON, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return Receipt{}, err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return Receipt{}, err
	}
	// Never remove a partially written output: preserve it for diagnosis.
	for name, data := range decoded {
		if err := writeExclusive(filepath.Join(output, name), data); err != nil {
			return Receipt{}, err
		}
	}
	if err := writeExclusive(filepath.Join(output, "receipt.json"), append(receiptJSON, '\n')); err != nil {
		return Receipt{}, err
	}
	directory, err := os.Open(output)
	if err != nil {
		return Receipt{}, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return Receipt{}, fmt.Errorf("output directory sync: %w", err)
	}
	return receipt, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return f.Sync()
}
