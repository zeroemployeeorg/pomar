package smoke

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/ocilayout"
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
	for _, n := range []string{"tmux", "libevent-core-2.1-7", "libutempter0"} {
		if names["libtinfo6"] {
			t.Fatal("libtinfo6 ships /lib paths, which break a merged-/usr image")
		}
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

// The qualification seat base is the CI image with its packages plus tmux,
// distinct from the CI base.
func TestTheSeatQualificationImageIsTheCIImageWithTmux(t *testing.T) {
	q, err := GuestImageByName("seat-qualification")
	if err != nil {
		t.Fatal(err)
	}
	ci, _ := GuestImageByName(DefaultGuestImage)
	if q.Digest != ci.Digest || q.Arm64 != ci.Arm64 || q.PackageSet() == ci.PackageSet() {
		t.Fatal("not the CI image with a distinct package set")
	}
	names := map[string]bool{}
	for _, p := range q.Packages {
		names[p.Name] = true
	}
	for _, n := range []string{"jq", "tmux", "libevent-core-2.1-7"} {
		if !names[n] {
			t.Fatalf("%s is missing", n)
		}
	}
}

// StageFiles must own payload bytes as well as its copied OCI metadata.
func TestStageFilesOwnsEveryReachedSourceBlob(t *testing.T) {
	hash := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(src, "blobs", "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := func(media string, data []byte) ocilayout.Descriptor {
		d := ocilayout.Descriptor{MediaType: media, Digest: "sha256:" + hash(data), Size: int64(len(data))}
		write(filepath.Join(src, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:")), data)
		return d
	}
	jsonBlob := func(media string, value any) ocilayout.Descriptor {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return blob(media, data)
	}
	write(filepath.Join(src, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`))
	var layers []ocilayout.Descriptor
	var diffIDs []string
	wanted := map[string]string{}
	for _, file := range []struct{ path, body string }{
		{"etc/os-release", "ID=debian\nVERSION_ID=12\n"},
		{"usr/bin/synthetic-tool", "synthetic payload; never executed\n"},
	} {
		var data bytes.Buffer
		tw := tar.NewWriter(&data)
		if err := tw.WriteHeader(&tar.Header{Name: file.path, Mode: 0o644, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		layers = append(layers, blob("application/vnd.oci.image.layer.v1.tar", data.Bytes()))
		diffIDs = append(diffIDs, "sha256:"+hash(data.Bytes()))
		wanted[file.path] = hash([]byte(file.body))
	}
	config := jsonBlob("application/vnd.oci.image.config.v1+json", map[string]any{
		"os": "linux", "architecture": "arm64",
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	manifest := jsonBlob(ocilayout.OCIManifest, map[string]any{
		"schemaVersion": 2, "mediaType": ocilayout.OCIManifest, "config": config, "layers": layers,
	})
	manifest.Platform = &ocilayout.Platform{OS: "linux", Architecture: "arm64"}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": ocilayout.OCIIndex, "manifests": []ocilayout.Descriptor{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(src, "index.json"), index)
	if _, err := ocilayout.CheckFiles(src, manifest.Digest, wanted); err != nil {
		t.Fatalf("source baseline: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "staged")
	if report, err := ocilayout.StageFiles(src, dst, manifest.Digest, wanted); err != nil || report == nil || len(report.Layers) != 2 {
		t.Fatalf("stage baseline: report=%+v, error=%v", report, err)
	}
	if _, err := ocilayout.CheckFiles(dst, manifest.Digest, wanted); err != nil {
		t.Fatalf("staged baseline: %v", err)
	}
	snapshot := map[string][]byte{}
	if err := filepath.WalkDir(dst, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		snapshot[path] = data
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range append([]ocilayout.Descriptor{manifest}, layers...) {
		path := filepath.Join(src, "blobs", "sha256", strings.TrimPrefix(descriptor.Digest, "sha256:"))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[0] ^= 1
		// Write in place: a rename would hide shared payload inodes.
		write(path, data)
		for stagedPath, original := range snapshot {
			current, err := os.ReadFile(stagedPath)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatalf("staged bytes changed after source mutation %s: %s, error=%v", descriptor.Digest, stagedPath, err)
			}
		}
		if _, err := ocilayout.CheckFiles(dst, manifest.Digest, wanted); err != nil {
			t.Fatalf("staged validation after source mutation %s: %v", descriptor.Digest, err)
		}
	}
}
