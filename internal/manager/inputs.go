package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Input is a file the client sends with a start request, as bytes: the
// manager never reads a path on the client's behalf. The helper copies the
// attempt's inputs into its guest at /pomar/inputs before the command is
// released (elders' ruling r14 §1: a document to lint goes in as a file).
type Input struct {
	Name   string `json:"name"`
	Data   []byte `json:"data"`
	SHA256 string `json:"sha256"` // of Data, as the client computed it
}

// InputRecord is what the attempt's entry keeps of an input.
type InputRecord struct {
	Name   string `json:"name"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Limits on inputs: they are documents and small artefacts, not sources.
const (
	maxInputs      = 16
	maxInputsBytes = 32 << 20
	inputsDir      = "inputs"
)

var inputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// checkInputs refuses bad names, duplicates, oversized sets and any input
// whose bytes do not hash to the sha256 the client sent.
func checkInputs(in []Input) ([]InputRecord, error) {
	if len(in) > maxInputs {
		return nil, fmt.Errorf("manager: %d inputs, at most %d", len(in), maxInputs)
	}
	seen, total := map[string]bool{}, 0
	var out []InputRecord
	for _, x := range in {
		if !inputName.MatchString(x.Name) || seen[x.Name] {
			return nil, fmt.Errorf("manager: invalid or repeated input name %q", x.Name)
		}
		seen[x.Name] = true
		total += len(x.Data)
		if total > maxInputsBytes {
			return nil, fmt.Errorf("manager: inputs over %d bytes", maxInputsBytes)
		}
		h := sha256.Sum256(x.Data)
		sum := hex.EncodeToString(h[:])
		if sum != x.SHA256 {
			return nil, fmt.Errorf("manager: input %s: sha256 %s, not the %s sent with it", x.Name, sum, x.SHA256)
		}
		out = append(out, InputRecord{Name: x.Name, Bytes: len(x.Data), SHA256: sum})
	}
	return out, nil
}

// writeInputs puts the inputs in the attempt's record and returns the
// directory, or "" when there are none.
func writeInputs(rec string, in []Input) (string, error) {
	if len(in) == 0 {
		return "", nil
	}
	dir := filepath.Join(rec, inputsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, x := range in {
		if err := os.WriteFile(filepath.Join(dir, x.Name), x.Data, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}
