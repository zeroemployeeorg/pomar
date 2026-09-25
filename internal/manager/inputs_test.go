package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func in(name, data string) Input {
	h := sha256.Sum256([]byte(data))
	return Input{Name: name, Data: []byte(data), SHA256: hex.EncodeToString(h[:])}
}

func TestCheckInputs(t *testing.T) {
	recs, err := checkInputs([]Input{in("doc.md", "hello"), in("CLAUDE.md", "x")})
	if err != nil || len(recs) != 2 || recs[0].Bytes != 5 || recs[0].SHA256 != in("", "hello").SHA256 {
		t.Fatalf("recs %+v, %v", recs, err)
	}
	bad := in("doc.md", "hello")
	bad.Data = []byte("hellO")
	for name, set := range map[string][]Input{
		"a changed file":  {bad},
		"a path":          {in("../x", "a")},
		"a slash":         {in("a/b", "a")},
		"a dot name":      {in(".hidden", "a")},
		"a repeated name": {in("a", "1"), in("a", "2")},
		"too large":       {in("big", strings.Repeat("x", maxInputsBytes+1))},
	} {
		if _, err := checkInputs(set); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := make([]Input, maxInputs+1)
	for i := range many {
		many[i] = in("f"+string(rune('a'+i)), "x")
	}
	if _, err := checkInputs(many); err == nil {
		t.Error("too many inputs accepted")
	}
}

func TestWriteInputs(t *testing.T) {
	rec := t.TempDir()
	dir, err := writeInputs(rec, []Input{in("doc.md", "the whole document")})
	if err != nil || dir != filepath.Join(rec, "inputs") {
		t.Fatalf("dir %q, %v", dir, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "doc.md"))
	if err != nil || string(b) != "the whole document" {
		t.Fatalf("content %q, %v", b, err)
	}
	if d, _ := writeInputs(rec, nil); d != "" {
		t.Fatalf("no inputs made a directory: %q", d)
	}
}

func TestStartRefusesInputsWithoutASource(t *testing.T) {
	m, _ := openAdmission(t, capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
		venue.Space{Used: 10 * capacity.GiB, Avail: 500 * capacity.GiB, Shared: true})
	if _, err := m.Start("a1", []string{"/bin/true"}, nil, in("doc.md", "x")); err == nil || !strings.Contains(err.Error(), "need a source") {
		t.Fatalf("Start = %v", err)
	}
}
