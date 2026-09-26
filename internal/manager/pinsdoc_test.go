package manager

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
)

func TestPinsDocIsCanonicalAndWrittenOnce(t *testing.T) {
	pins := &Pins{Kernel: "k", Init: "i", Image: "img", Pomar: "v", HostBin: "hb", CommandHash: "ch", RootfsSHA256: strings.Repeat("b", 64)}
	src := pinnedSource(&Source{Mirror: "example", Ref: "main", Git: true, ReadOnly: true}, strings.Repeat("a", 40))
	doc, err := pinsDoc("p-1", capacity.CI, pins, src)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := pinsDoc("p-1", capacity.CI, pins, src)
	if !bytes.Equal(doc, again) {
		t.Fatal("the pins document is not deterministic")
	}
	var got struct {
		Schema  string        `json:"schema"`
		Attempt string        `json:"attempt"`
		Pins    Pins          `json:"pins"`
		Source  *PinnedSource `json:"source"`
	}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != PinsSchema || got.Attempt != "p-1" || got.Pins != *pins || *got.Source != *src || got.Source.Base != "main" {
		t.Fatalf("pins document = %s", doc)
	}
	rec := t.TempDir()
	p, err := writePins(rec, doc)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o400 {
		t.Fatalf("mode %v, want 0400", fi.Mode().Perm())
	}
	if _, err := writePins(rec, doc); err == nil {
		t.Fatal("the pins document was written twice")
	}
}

// `pomar attempt pins` gets the exact bytes in the record.
func TestPinsRouteServesTheExactBytes(t *testing.T) {
	_, ctl, v := openSigning(t, false)
	rec := filepath.Join(v.Root(), attemptsDir, "p-2")
	os.MkdirAll(rec, 0o700)
	doc, _ := pinsDoc("p-2", capacity.CI, &Pins{Kernel: "k"}, nil)
	if _, err := writePins(rec, doc); err != nil {
		t.Fatal(err)
	}
	b, err := ctl.Pins("p-2")
	if err != nil || !bytes.Equal(b, doc) {
		t.Fatalf("Pins = %q, %v; want %q", b, err, doc)
	}
	if _, err := ctl.Pins("none"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Pins(none) = %v, want 404", err)
	}
}

func TestResultCarriesTheRootfsHash(t *testing.T) {
	m := &Manager{}
	e := terminalEntry("r")
	if _, ok := m.resultDoc(e)["rootfs_sha256"]; ok {
		t.Fatal("rootfs_sha256 on an attempt with no base")
	}
	e.Pins.RootfsSHA256 = strings.Repeat("c", 64)
	if m.resultDoc(e)["rootfs_sha256"] != strings.Repeat("c", 64) {
		t.Fatal("rootfs_sha256 missing")
	}
}
