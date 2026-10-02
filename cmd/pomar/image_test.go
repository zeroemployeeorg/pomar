package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestImageNameMustBeTaggedAndNotPomarsOwn(t *testing.T) {
	for _, ok := range []string{"example.org/ci:20260926-fonts", "localhost:5000/team/ci:1", "ci:v2"} {
		if err := checkImageName(ok); err != nil {
			t.Errorf("checkImageName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"example.org/ci", "localhost:5000/team/ci", "example.org/ci:", "example.org/ci@sha256:ab",
		"docker.io/library/golang:any", "ghcr.io/apple/containerization/vminit:0.45.0", "docker.io/library/node:24-bookworm-slim"} {
		if err := checkImageName(bad); err == nil {
			t.Errorf("checkImageName(%q) was accepted", bad)
		}
	}
}

func TestImageCommandsRefuseAMalformedPin(t *testing.T) {
	for _, args := range [][]string{
		{"image", "check", "-layout", t.TempDir(), "-digest", "5670320a"},
		{"image", "load", "-layout", t.TempDir(), "-digest", "sha256:12", "-host-bin", "/x", "-name", "a:b"},
	} {
		var out, errb bytes.Buffer
		if got := run(args, &out, &errb); got != 2 || !strings.Contains(errb.String(), "not a sha256 digest") {
			t.Errorf("run(%v) = %d, stderr %q", args, got, errb.String())
		}
	}
}

func TestImageCheckOfAMissingLayoutFails(t *testing.T) {
	var out, errb bytes.Buffer
	pin := "sha256:" + strings.Repeat("0", 64)
	if got := run([]string{"image", "check", "-layout", t.TempDir(), "-digest", pin}, &out, &errb); got != 1 || !strings.Contains(errb.String(), "oci-layout") {
		t.Fatalf("run = %d, stderr %q", got, errb.String())
	}
}

// Allow lists resolve accounts by name or uid, and refuse one that does not exist.
func TestLookupUID(t *testing.T) {
	if uid, err := lookupUID("root"); err != nil || uid != 0 {
		t.Fatalf("root: %d, %v", uid, err)
	}
	if uid, err := lookupUID("0"); err != nil || uid != 0 {
		t.Fatalf("0: %d, %v", uid, err)
	}
	if _, err := lookupUID("no-such-account-pomar"); err == nil {
		t.Fatal("an unknown account resolved")
	}
}

// A class's image and a base build name an image in the catalogue, never a
// reference; anything else is refused before anything is opened.
func TestGuestImageIsChosenFromTheCatalogue(t *testing.T) {
	for _, args := range [][]string{
		{"manager", "-host-bin", "/nonexistent", "-kernel-sha256", "00", "-class-image", "docker.io/library/node:24"},
		{"base", "build", "-host-bin", "/nonexistent", "-image", "nope"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "no pinned guest image") || !strings.Contains(errb.String(), "node24-slim") {
			t.Errorf("%v: exit %d, %q", args, code, errb.String())
		}
	}
}
