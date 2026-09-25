package manager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandHash(t *testing.T) {
	a := CommandHash([]string{"make", "verify"})
	if a != CommandHash([]string{"make", "verify"}) {
		t.Fatal("not deterministic")
	}
	for _, other := range [][]string{{"make", "test"}, {"make verify"}, {"make", "verify", ""}} {
		if CommandHash(other) == a {
			t.Fatalf("%q hashes like make verify", other)
		}
	}
	// The hash of ["make","verify"], computed independently with shasum.
	if a != "12cc93533c93ea3fd91492433d84cf2be19922c260efb5b3f482c5b852d5f347" {
		t.Fatalf("hash %q", a)
	}
}

func TestPinsFor(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pomar-host")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{HostBin: bin, Guest: Guest{KernelSHA256: "k", InitDigest: "i", ImageDigest: "d",
		ImageArm64: "a", PackageSet: "p"}}}
	p, err := m.pinsFor([]string{"make", "verify"})
	if err != nil {
		t.Fatal(err)
	}
	want := Pins{Kernel: "k", Init: "i", Image: "d", ImageArm64: "a", PackageSet: "p",
		HostBin: "9a3a45d01531a20e89ac6ae10b0b0beb0492acd7216a368aa062d1a5fecaf9cd",
		CommandHash: CommandHash([]string{"make", "verify"}), Pomar: p.Pomar}
	if *p != want || p.Pomar == "" {
		t.Fatalf("pins %+v, want %+v", *p, want)
	}
	m.cfg.HostBin = filepath.Join(t.TempDir(), "missing")
	if _, err := m.pinsFor(nil); err == nil {
		t.Fatal("a missing helper binary was pinned")
	}
}
