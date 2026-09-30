package ocilayout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// file is one tar entry in a test layer.
type file struct {
	name, body string
}

func tarOf(t *testing.T, files ...file) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write(b)
	z.Close()
	return buf.Bytes()
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// layout builds an OCI layout in a temp dir, one blob at a time.
type layout struct {
	t   *testing.T
	dir string
}

func newLayout(t *testing.T) *layout {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "layout")
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600)
	return &layout{t: t, dir: dir}
}

func (l *layout) blob(mediaType string, b []byte) Descriptor {
	d := Descriptor{MediaType: mediaType, Digest: digest(b), Size: int64(len(b))}
	os.WriteFile(l.path(d), b, 0o600)
	return d
}

func (l *layout) path(d Descriptor) string {
	return filepath.Join(l.dir, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
}

func (l *layout) json(mediaType string, v any) Descriptor {
	b, _ := json.Marshal(v)
	return l.blob(mediaType, b)
}

func (l *layout) root(d Descriptor) {
	d.Annotations = map[string]string{"org.opencontainers.image.ref.name": "example.org/ci:1"}
	b, _ := json.Marshal(index{MediaType: OCIIndex, Manifests: []Descriptor{d}})
	os.WriteFile(filepath.Join(l.dir, "index.json"), b, 0o600)
}

// image writes a config and manifest over the given uncompressed layers (each
// stored with the given media type, gzipped if it says so) and returns the
// manifest's descriptor.
type imageOpts struct {
	os, arch    string
	env         []string
	gzip        bool
	wrongDiffID bool
}

func (l *layout) image(o imageOpts, layers ...[]byte) Descriptor {
	if o.os == "" {
		o.os, o.arch = "linux", "arm64"
	}
	var cfg config
	cfg.OS, cfg.Architecture, cfg.Config.Env = o.os, o.arch, o.env
	cfg.RootFS.Type = "layers"
	var descs []Descriptor
	for _, raw := range layers {
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, digest(raw))
		if o.gzip {
			descs = append(descs, l.blob("application/vnd.oci.image.layer.v1.tar+gzip", gz(l.t, raw)))
		} else {
			descs = append(descs, l.blob("application/vnd.oci.image.layer.v1.tar", raw))
		}
	}
	if o.wrongDiffID {
		cfg.RootFS.DiffIDs[0] = digest([]byte("not this layer"))
	}
	c := l.json("application/vnd.oci.image.config.v1+json", cfg)
	return l.json(OCIManifest, manifest{MediaType: OCIManifest, Config: c, Layers: descs})
}

func (l *layout) attestation() Descriptor {
	c := l.blob("application/vnd.oci.image.config.v1+json", []byte(`{}`))
	s := l.blob("application/vnd.in-toto+json", []byte(`{"predicateType":"x"}`))
	d := l.json(OCIManifest, manifest{MediaType: OCIManifest, Config: c, Layers: []Descriptor{s}})
	d.Platform = &Platform{OS: "unknown", Architecture: "unknown"}
	return d
}

func arm64(d Descriptor) Descriptor {
	d.Platform = &Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	return d
}

var osRelease = file{"etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n"}

func TestSingleManifestLayoutIsAccepted(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{env: []string{"PATH=/usr/bin:/bin", "NODE_VERSION=22.1.0"}},
		tarOf(t, osRelease, file{"usr/bin/node", "x"}), tarOf(t, file{"opt/app/fonts/a.ttf", "font"}))
	l.root(m)
	r, err := Check(l.dir, m.Digest)
	if err != nil {
		t.Fatalf("Check: %v (%+v)", err, r)
	}
	if r.Manifest != m.Digest || r.Platform != "linux/arm64" || len(r.Layers) != 2 || r.Reference != "example.org/ci:1" {
		t.Fatalf("report: %+v", r)
	}
	if r.OSRelease["ID"] != "debian" || r.OSRelease["VERSION_ID"] != "12" {
		t.Fatalf("os-release: %v", r.OSRelease)
	}
	if strings.Join(r.Env, " ") != "PATH=/usr/bin:/bin NODE_VERSION=22.1.0" {
		t.Fatalf("env: %v", r.Env)
	}
	if r.Blobs != 4 { // manifest, config, two layers
		t.Fatalf("blobs = %d", r.Blobs)
	}
}

func TestIndexWithAttestationAndGzipLayersIsAccepted(t *testing.T) {
	l := newLayout(t)
	m := arm64(l.image(imageOpts{gzip: true}, tarOf(t, osRelease)))
	idx := l.json(OCIIndex, index{MediaType: OCIIndex, Manifests: []Descriptor{m, l.attestation()}})
	l.root(idx)
	r, err := Check(l.dir, m.Digest)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if r.Root != idx.Digest || r.Manifest != m.Digest || r.Layers[0].DiffID == r.Layers[0].Digest {
		t.Fatalf("report: %+v", r)
	}
}

func TestAnotherPinIsRefused(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{}, tarOf(t, osRelease))
	l.root(m)
	r, err := Check(l.dir, digest([]byte("another image")))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not the pinned") || r == nil {
		t.Fatalf("err = %v", err)
	}
}

func TestAnAmd64ConfigIsRefused(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{os: "linux", arch: "amd64"}, tarOf(t, osRelease))
	l.root(m)
	_, err := Check(l.dir, m.Digest)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "linux/amd64, not linux/arm64") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnotherPlatformInTheIndexIsRefused(t *testing.T) {
	l := newLayout(t)
	m := arm64(l.image(imageOpts{}, tarOf(t, osRelease)))
	other := l.image(imageOpts{os: "linux", arch: "amd64"}, tarOf(t, osRelease))
	other.Platform = &Platform{OS: "linux", Architecture: "amd64"}
	l.root(l.json(OCIIndex, index{MediaType: OCIIndex, Manifests: []Descriptor{m, other}}))
	_, err := Check(l.dir, m.Digest)
	if err == nil || !strings.Contains(err.Error(), "another platform (linux/amd64)") {
		t.Fatalf("err = %v", err)
	}
}

func TestAChangedOrMissingBlobIsRefused(t *testing.T) {
	l := newLayout(t)
	layer := tarOf(t, osRelease)
	m := l.image(imageOpts{}, layer)
	l.root(m)
	p := filepath.Join(l.dir, "blobs", "sha256", strings.TrimPrefix(digest(layer), "sha256:"))
	changed := append([]byte{}, layer...)
	changed[600] ^= 1
	os.WriteFile(p, changed, 0o600)
	if _, err := Check(l.dir, m.Digest); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("changed: err = %v", err)
	}
	os.Remove(p)
	if _, err := Check(l.dir, m.Digest); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing: err = %v", err)
	}
}

func TestADiffIDMismatchIsRefused(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{wrongDiffID: true}, tarOf(t, osRelease))
	l.root(m)
	_, err := Check(l.dir, m.Digest)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not its diff ID") {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialFilesAreRefusedAndNamed(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{},
		tarOf(t, osRelease, file{"root/.npmrc", "//registry/:_authToken=x"}, file{"home/ci/.ssh/id_ed25519", "k"},
			file{"root/.docker/config.json", "{}"}, file{"etc/.netrc", "m"}, file{"usr/lib/node_modules/x/.gitignore", ""}),
		tarOf(t, file{"root/.wh..npmrc", ""}, file{"home/ci/.git-credentials", "u"}))
	l.root(m)
	r, err := Check(l.dir, m.Digest)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "credential file") {
		t.Fatalf("err = %v", err)
	}
	want := "etc/.netrc home/ci/.git-credentials home/ci/.ssh/id_ed25519 root/.docker/config.json root/.npmrc"
	if got := strings.Join(r.CredentialPaths, " "); got != want {
		t.Fatalf("credential paths:\n got %s\nwant %s", got, want)
	}
}

func TestCredentialsInEnvAreRefusedAndNotRecorded(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{env: []string{"PATH=/bin", "GITHUB_TOKEN=abc123", "NPM_CONFIG_REGISTRY=npm_s3cr3tvalue"}}, tarOf(t, osRelease))
	l.root(m)
	r, err := Check(l.dir, m.Digest)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "NPM_CONFIG_REGISTRY") {
		t.Fatalf("err = %v", err)
	}
	b, _ := json.Marshal(r)
	if bytes.Contains(b, []byte("abc123")) || bytes.Contains(b, []byte("s3cr3t")) {
		t.Fatalf("a refused value was recorded: %s", b)
	}
	if strings.Join(r.Env, " ") != "PATH=/bin GITHUB_TOKEN=<refused> NPM_CONFIG_REGISTRY=<refused>" {
		t.Fatalf("env: %v", r.Env)
	}
}

func TestMoreThanOneRootIsRefused(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{}, tarOf(t, osRelease))
	b, _ := json.Marshal(index{Manifests: []Descriptor{m, m}})
	os.WriteFile(filepath.Join(l.dir, "index.json"), b, 0o600)
	if _, err := Check(l.dir, m.Digest); err == nil || !strings.Contains(err.Error(), "2 roots") {
		t.Fatalf("err = %v", err)
	}
}

func TestStageCopiesOnlyWhatTheRootReaches(t *testing.T) {
	l := newLayout(t)
	m := arm64(l.image(imageOpts{gzip: true}, tarOf(t, osRelease)))
	l.root(l.json(OCIIndex, index{MediaType: OCIIndex, Manifests: []Descriptor{m, l.attestation()}}))
	stray := l.blob("application/octet-stream", []byte("not reached from the root"))
	dst := filepath.Join(t.TempDir(), "staged")
	r, err := Stage(l.dir, dst, m.Digest)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dst, "blobs", "sha256"))
	if len(entries) != r.Blobs || r.Blobs != 7 { // index, manifest, config, layer; attestation manifest, config, statement
		t.Fatalf("staged %d blobs, report %d", len(entries), r.Blobs)
	}
	if _, err := os.Stat(filepath.Join(dst, "blobs", "sha256", strings.TrimPrefix(stray.Digest, "sha256:"))); err == nil {
		t.Fatal("a blob the root does not reach was copied")
	}
	if _, err := Stage(l.dir, dst, m.Digest); err == nil {
		t.Fatal("Stage into an existing directory was allowed")
	}
}

func TestStageRefusesASymlinkedBlob(t *testing.T) {
	l := newLayout(t)
	layer := tarOf(t, osRelease)
	m := l.image(imageOpts{}, layer)
	l.root(m)
	p := filepath.Join(l.dir, "blobs", "sha256", strings.TrimPrefix(digest(layer), "sha256:"))
	outside := filepath.Join(t.TempDir(), "outside")
	os.Rename(p, outside)
	os.Symlink(outside, p)
	if _, err := Stage(l.dir, filepath.Join(t.TempDir(), "staged"), m.Digest); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPinThatIsNotSha256IsRefused(t *testing.T) {
	if _, err := Check(t.TempDir(), "5670320a"); err == nil {
		t.Fatal("a short pin was accepted")
	}
}

func TestOnlyZeroPaddingMayFollowTheTarEnd(t *testing.T) {
	base := tarOf(t, osRelease)
	for _, c := range []struct {
		name   string
		layer  []byte
		gzip   bool
		refuse bool
	}{
		{"zero padding", append(append([]byte{}, base...), make([]byte, 8192)...), false, false},
		{"bytes after the marker", append(append([]byte{}, base...), []byte("root/.npmrc _authToken=x")...), false, true},
		{"a second gzip member", nil, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLayout(t)
			var m Descriptor
			if c.gzip {
				// Two gzip members: the second archive would never be unpacked,
				// but its bytes are in the blob.
				second := tarOf(t, file{"root/.npmrc", "//registry/:_authToken=x"})
				raw := append(append([]byte{}, base...), second...)
				blob := append(gz(t, base), gz(t, second)...)
				layer := l.blob("application/vnd.oci.image.layer.v1.tar+gzip", blob)
				var cfg config
				cfg.OS, cfg.Architecture, cfg.RootFS.Type, cfg.RootFS.DiffIDs = "linux", "arm64", "layers", []string{digest(raw)}
				cd := l.json("application/vnd.oci.image.config.v1+json", cfg)
				m = l.json(OCIManifest, manifest{MediaType: OCIManifest, Config: cd, Layers: []Descriptor{layer}})
			} else {
				m = l.image(imageOpts{}, c.layer)
			}
			l.root(m)
			_, err := Check(l.dir, m.Digest)
			switch {
			case c.refuse && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "after its tar end-of-archive marker")):
				t.Fatalf("err = %v", err)
			case !c.refuse && err != nil:
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOnlyAnEmptyRegularFileIsAWhiteout(t *testing.T) {
	l := newLayout(t)
	m := l.image(imageOpts{}, tarOf(t, osRelease, file{"root/.ssh/.wh.payload", "not a whiteout"}, file{"etc/.wh.gone", ""}))
	l.root(m)
	r, err := Check(l.dir, m.Digest)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "root/.ssh/.wh.payload is named as a whiteout but is not an empty regular file") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(r.CredentialPaths, " ") != "root/.ssh/.wh.payload" || strings.Contains(err.Error(), "etc/.wh.gone") {
		t.Fatalf("credential paths %v, err %v", r.CredentialPaths, err)
	}
}

// entry is a tar entry of any type, for the files tests.
type entry struct {
	name, body, link string
	typ              byte
}

func tarEntries(t *testing.T, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Listed files are judged in the image's final filesystem: the last layer's
// bytes, whiteouts and opaque directories applied, and a later symlink over a
// parent directory hiding the file.
func TestFilesAreCheckedInTheFinalFilesystem(t *testing.T) {
	const fonts = "opt/app/fonts/"
	l := newLayout(t)
	m := l.image(imageOpts{},
		tarEntries(t, entry{name: "etc/os-release", body: osRelease.body},
			entry{name: fonts + "keep.ttf", body: "keep"}, entry{name: fonts + "changed.ttf", body: "v1"},
			entry{name: fonts + "gone.ttf", body: "gone"}, entry{name: fonts + "back.ttf", body: "back"},
			entry{name: "opt/opq/in.ttf", body: "in"}, entry{name: "opt/moved/x.ttf", body: "x"},
			entry{name: fonts + "link.ttf", typ: tar.TypeSymlink, link: "keep.ttf"}),
		tarEntries(t, entry{name: fonts + "changed.ttf", body: "v2"}, entry{name: fonts + ".wh.gone.ttf"},
			entry{name: fonts + ".wh.back.ttf"}, entry{name: "opt/opq/.wh..wh..opq"},
			entry{name: "opt/moved", typ: tar.TypeSymlink, link: "/elsewhere"}),
		tarEntries(t, entry{name: fonts + "back.ttf", body: "back"}))
	l.root(m)
	want := map[string]string{
		fonts + "keep.ttf": sum("keep"), fonts + "changed.ttf": sum("v1"), fonts + "gone.ttf": sum("gone"),
		fonts + "back.ttf": sum("back"), "opt/opq/in.ttf": sum("in"), "opt/moved/x.ttf": sum("x"),
		fonts + "link.ttf": sum("keep"), fonts + "never.ttf": sum("never"), "etc/os-release": sum(osRelease.body),
	}
	r, err := CheckFiles(l.dir, m.Digest, want)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "6 of 9 listed files are not as listed") {
		t.Fatalf("err = %v", err)
	}
	got := map[string]string{}
	for _, f := range r.Files {
		got[f.Path] = f.Result
	}
	for p, res := range map[string]string{
		fonts + "keep.ttf":    "ok",
		fonts + "changed.ttf": "different bytes",
		fonts + "gone.ttf":    "missing",
		fonts + "back.ttf":    "ok", // removed, then added again
		"opt/opq/in.ttf":      "missing",
		"opt/moved/x.ttf":     "not a regular file: under a symbolic link at opt/moved",
		fonts + "link.ttf":    "not a regular file: symbolic link",
		fonts + "never.ttf":   "missing",
		"etc/os-release":      "ok", // read for os-release and checked too
	} {
		if got[p] != res {
			t.Errorf("%s: %q, want %q", p, got[p], res)
		}
	}
	if r.OSRelease["ID"] != "debian" {
		t.Fatalf("os-release not read alongside the check: %v", r.OSRelease)
	}
	// All as listed: accepted, and StageFiles checks the staged copy the same way.
	ok := map[string]string{fonts + "keep.ttf": sum("keep"), fonts + "changed.ttf": sum("v2"), fonts + "back.ttf": sum("back")}
	if _, err := CheckFiles(l.dir, m.Digest, ok); err != nil {
		t.Fatalf("all as listed: %v", err)
	}
	if _, err := StageFiles(l.dir, filepath.Join(t.TempDir(), "staged"), m.Digest, want); !errors.Is(err, ErrRefused) {
		t.Fatalf("StageFiles: %v", err)
	}
}

func TestParseFilesManifest(t *testing.T) {
	a, b := sum("a"), sum("b")
	m, err := ParseFilesManifest(strings.NewReader(a+"  DejaVuSans.ttf\n"+b+" *sub/Noto.ttf\n\n"+a+"  /etc/abs.ttf\r\n"), "/opt/app/fonts")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m["opt/app/fonts/DejaVuSans.ttf"] != a || m["opt/app/fonts/sub/Noto.ttf"] != b || m["etc/abs.ttf"] != a {
		t.Fatalf("parsed %v", m)
	}
	for name, tc := range map[string]struct{ in, root string }{
		"a short hash":             {"abc  x.ttf\n", "/r"},
		"one space":                {a + " x.ttf\n", "/r"},
		"uppercase hex":            {strings.ToUpper(a) + "  x.ttf\n", "/r"},
		"a dotdot path":            {a + "  ../x.ttf\n", "/r"},
		"relative with no root":    {a + "  x.ttf\n", ""},
		"a relative root":          {a + "  x.ttf\n", "r"},
		"one path with two hashes": {a + "  x.ttf\n" + b + "  x.ttf\n", "/r"},
		"nothing listed":           {"\n\n", "/r"},
	} {
		if _, err := ParseFilesManifest(strings.NewReader(tc.in), tc.root); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
