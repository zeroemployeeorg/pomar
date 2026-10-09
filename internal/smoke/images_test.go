package smoke

import (
	"strings"
	"testing"
)

// The catalogue is consistent, and its first image is the CI image as it
// was before the catalogue existed: the CI class's base key is unchanged.
func TestGuestImagesCatalogue(t *testing.T) {
	if err := checkGuestImages(GuestImages); err != nil {
		t.Fatal(err)
	}
	ci, err := GuestImageByName(DefaultGuestImage)
	if err != nil || GuestImages[0].Name != DefaultGuestImage {
		t.Fatalf("the default image: %+v, %v", ci, err)
	}
	if ci.Ref() != ImageRepo+"@"+ImageDigest || ci.Arm64 != ImageArm64 || ci.PackageSet() == "" {
		t.Fatalf("the CI image moved: %+v", ci)
	}
	node, err := GuestImageByName("node24-slim")
	if err != nil || node.PackageSet() != "" || !strings.HasPrefix(node.Ref(), "docker.io/library/node@sha256:") {
		t.Fatalf("node24-slim: %+v, %v", node, err)
	}
	full, err := GuestImageByName("node24-full")
	if err != nil || full.PackageSet() != "" || full.Repo != node.Repo || full.Arm64 == node.Arm64 {
		t.Fatalf("node24-full: %+v, %v", full, err)
	}
	if _, err := GuestImageByName("docker.io/library/node:24"); err == nil || !strings.Contains(err.Error(), "golang-ci, node24-slim, node24-full") {
		t.Fatalf("a reference instead of a name: %v", err)
	}
	local, err := GuestImageByName("node24-python314-chromium")
	if err != nil || !local.LocalLayout || local.Ref() != "localhost/pomar/runner-tools:20261007-v5" || local.PackageSet() != "" {
		t.Fatalf("local tools image: %+v, %v", local, err)
	}
}

// The consistency check refuses what a bad edit to the catalogue would add.
func TestCheckGuestImagesRefusals(t *testing.T) {
	good := GuestImage{Name: "a", Repo: "r", Tag: "t", Digest: ImageDigest, Arm64: ImageArm64}
	for name, images := range map[string][]GuestImage{
		"a duplicate name":    {good, func() GuestImage { g := good; g.Arm64 = InitDigest; return g }()},
		"a name with a slash": {func() GuestImage { g := good; g.Name = "a/b"; return g }()},
		"a short digest":      {func() GuestImage { g := good; g.Digest = "sha256:abc"; return g }()},
		"no tag":              {func() GuestImage { g := good; g.Tag = ""; return g }()},
		"the same base twice": {good, func() GuestImage { g := good; g.Name = "b"; return g }()},
	} {
		if err := checkGuestImages(images); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The seat image is the runner tools plus tmux and exactly its libraries,
// as its own base, distinct from the runner tools' own.
func TestTheSeatImageCarriesTmux(t *testing.T) {
	seat, err := GuestImageByName("seat-runner-tools")
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := GuestImageByName("node24-python314-chromium")
	if seat.Digest != tools.Digest || seat.Arm64 != tools.Arm64 || seat.Ref() != tools.Ref() {
		t.Fatal("the seat image isn't the runner tools' image")
	}
	names := map[string]bool{}
	for _, p := range seat.Packages {
		names[p.Name] = true
		if !strings.HasPrefix(p.URL, "https://deb.debian.org/debian/pool/") || len(p.SHA256) != 64 {
			t.Fatalf("package %s isn't pinned on the archive", p.Name)
		}
	}
	for _, n := range []string{"tmux", "libevent-core-2.1-7", "libutempter0", "libtinfo6"} {
		if !names[n] {
			t.Fatalf("%s is missing", n)
		}
	}
	if seat.PackageSet() == "" || seat.PackageSet() == tools.PackageSet() {
		t.Fatal("the seat image's base isn't distinct")
	}
	if err := checkGuestImages(GuestImages); err != nil {
		t.Fatal(err)
	}
}
