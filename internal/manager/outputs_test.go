package manager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestCheckOutputs(t *testing.T) {
	if err := checkOutputs([]string{"report.json", "log_1.txt"}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, maxOutputs+1)
	for i := range many {
		many[i] = "f" + string(rune('a'+i))
	}
	for name, set := range map[string][]string{
		"a path":          {"../x"},
		"a slash":         {"a/b"},
		"a dot name":      {".pomar-stage"},
		"a space":         {"a b"},
		"a shell word":    {"$(x)"},
		"a repeated name": {"a", "a"},
		"an empty name":   {""},
		"too many":        many,
	} {
		if err := checkOutputs(set); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOutputArgs(t *testing.T) {
	if a := outputArgs("/r", nil); a != nil {
		t.Fatalf("no outputs gave %v", a)
	}
	want := []string{"--outputs", "a,b", "--outputs-dir", "/r/outputs", "--outputs-max", "67108864"}
	if a := outputArgs("/r", []string{"a", "b"}); !reflect.DeepEqual(a, want) {
		t.Fatalf("args = %v", a)
	}
}

func TestStartRefusesOutputsWithoutASource(t *testing.T) {
	m, _ := openAdmission(t, capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
		venue.Space{Used: 10 * capacity.GiB, Avail: 500 * capacity.GiB, Shared: true})
	if _, err := m.StartOut("", "a1", []string{"/bin/true"}, nil, []string{"report.json"}); err == nil || !strings.Contains(err.Error(), "need a source") {
		t.Fatalf("StartOut = %v", err)
	}
}

// collectOutputs keeps what it can check, whatever the guest sent: a regular
// file within the cap is hashed and made read-only; anything else is removed.
func TestCollectOutputs(t *testing.T) {
	rec := t.TempDir()
	dir := filepath.Join(rec, outputsDir)
	os.MkdirAll(filepath.Join(dir, "adir", "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"ok":true}`), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
	// Sparse files: sizes over the cap without the bytes.
	big := func(name string, size int64) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(size)
		f.Close()
	}
	big("huge", MaxOutputsBytes+1)
	big("first", 40<<20)
	big("second", 40<<20) // within the cap alone, over it after first
	os.WriteFile(filepath.Join(dir, "undeclared"), []byte("x"), 0o600)

	got := collectOutputs(rec, []string{"report.json", "missing", "link", "adir", "huge", "first", "second"})
	byName := map[string]OutputRecord{}
	for _, o := range got {
		byName[o.Name] = o
	}
	if len(got) != 7 {
		t.Fatalf("records = %+v", got)
	}
	if r := byName["report.json"]; r.Status != OutputOK || r.Bytes != 11 || r.SHA256 != sha(`{"ok":true}`) {
		t.Fatalf("report.json = %+v", r)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "report.json")); fi.Mode().Perm() != 0o400 {
		t.Fatalf("report.json mode %v, want read-only", fi.Mode().Perm())
	}
	if byName["first"].Status != OutputOK || byName["first"].Bytes != 40<<20 {
		t.Fatalf("first = %+v", byName["first"])
	}
	for name, want := range map[string]string{
		"missing": OutputMissing, "link": OutputNotRegular, "adir": OutputNotRegular,
		"huge": OutputOverCap, "second": OutputOverCap,
	} {
		if r := byName[name]; r.Status != want || r.SHA256 != "" || r.Bytes != 0 {
			t.Errorf("%s = %+v, want %s", name, r, want)
		}
		if name != "missing" {
			if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("%s left in the record: %v", name, err)
			}
		}
	}
	// A symlink's target is never touched.
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatal(err)
	}
}

// An attempt that ends records its outputs in its entry and its result, and
// serves each recorded one, checked, on the control socket.
func TestFinishRecordsAndServesOutputs(t *testing.T) {
	m, ctl, v := openSigning(t, false)
	id := "out-1"
	rec := filepath.Join(v.Root(), attemptsDir, id)
	os.MkdirAll(filepath.Join(rec, outputsDir), 0o700)
	os.WriteFile(filepath.Join(rec, outputsDir, "report.json"), []byte("the report"), 0o600)
	e := terminalEntry(id)
	e.State, e.ExitCode, e.OutputNames = StateRunning, nil, []string{"report.json", "absent.txt"}
	m.mu.Lock()
	m.t.entries[id] = &e
	m.mu.Unlock()
	code := 0
	m.finish(id, StateExited, "", &code)

	want := []OutputRecord{
		{Name: "report.json", Status: OutputOK, Bytes: 10, SHA256: sha("the report")},
		{Name: "absent.txt", Status: OutputMissing},
	}
	var got Entry
	for _, x := range m.List() {
		if x.Attempt == id {
			got = x
		}
	}
	if !reflect.DeepEqual(got.Outputs, want) {
		t.Fatalf("entry outputs = %+v", got.Outputs)
	}
	var r ResultReply
	if err := ctl.Do("GET", "/v1/attempts/"+id+"/result", nil, &r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Outputs []OutputRecord `json:"outputs"`
	}
	if err := json.Unmarshal(r.Result, &doc); err != nil || !reflect.DeepEqual(doc.Outputs, want) {
		t.Fatalf("result outputs = %+v, %v", doc.Outputs, err)
	}

	var buf bytes.Buffer
	if s, err := ctl.Output(id, "report.json", &buf); err != nil || s != sha("the report") || buf.String() != "the report" {
		t.Fatalf("Output = %q %q %v", s, buf.String(), err)
	}
	for _, name := range []string{"absent.txt", "undeclared"} {
		if _, err := ctl.Output(id, name, &buf); err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("Output(%s) = %v, want 404", name, err)
		}
	}
	if _, err := ctl.Output(id, "..", &buf); err == nil {
		t.Error("Output(..) served")
	}
}

// An attempt without outputs keeps the result document it had.
func TestResultWithoutOutputsHasNoOutputsField(t *testing.T) {
	m := &Manager{}
	if _, ok := m.resultDoc(terminalEntry("x"))["outputs"]; ok {
		t.Fatal("outputs field on an attempt that named none")
	}
}
