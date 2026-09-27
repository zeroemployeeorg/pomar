// Package ocilayout checks an OCI image layout before Pomar loads it, so an
// image that no registry holds can still be pinned by a content address
// anyone can check: its linux/arm64 manifest's digest.
//
// Stage copies a layout into a directory Pomar owns, hashing every blob as it
// copies, and then checks the copy; the copy is what gets loaded, so nothing
// changes between the check and the load. A layout is accepted only if:
//
//   - index.json names exactly one root, and the one platform manifest under
//     it is linux/arm64 and is the pinned digest (attestation manifests, with
//     platform unknown/unknown, may sit beside it; any other platform is
//     refused);
//   - every blob reached from the root is present, of its stated size, and
//     hashes to its name (sha256 only);
//   - the config says linux/arm64, and each layer, uncompressed, hashes to
//     its diff ID;
//   - no layer holds a credential file (.npmrc, .netrc, .git-credentials,
//     anything under .ssh/, .docker/config.json), even one a later layer
//     deletes, since its bytes still ship in the image;
//   - no variable in the config's Env looks like a credential, by its name or
//     by a known token prefix in its value.
//
// A refusal is reported, never cleaned: Pomar does not edit an image.
package ocilayout

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Media types the checker understands.
const (
	OCIIndex       = "application/vnd.oci.image.index.v1+json"
	OCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	DockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	DockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// maxJSON caps every index, manifest and config the checker reads whole.
const maxJSON = 4 << 20

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform is a descriptor's platform.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type index struct {
	MediaType string       `json:"mediaType"`
	Manifests []Descriptor `json:"manifests"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    Descriptor   `json:"config"`
	Layers    []Descriptor `json:"layers"`
}

type config struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
	Config       struct {
		Env []string `json:"Env"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Layer is one checked layer.
type Layer struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	DiffID    string `json:"diff_id"`
}

// Report is what a check read. It is written whether or not the layout was
// refused, so a refusal can be shown with its evidence. A refused Env value
// is never recorded: its entry reads NAME=<refused>.
type Report struct {
	Manifest        string            `json:"arm64_manifest"`
	Root            string            `json:"root"`
	RootMediaType   string            `json:"root_media_type"`
	Reference       string            `json:"reference,omitempty"`
	Config          string            `json:"config"`
	Platform        string            `json:"platform"`
	Layers          []Layer           `json:"layers"`
	Blobs           int               `json:"blobs"`
	Bytes           int64             `json:"bytes"`
	Env             []string          `json:"env"`
	OSRelease       map[string]string `json:"os_release,omitempty"`
	CredentialPaths []string          `json:"credential_paths,omitempty"`
	Refusals        []string          `json:"refusals,omitempty"`
}

// ErrRefused marks a layout that was read in full and refused; the report
// names why.
var ErrRefused = errors.New("ocilayout: refused")

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Stage copies the layout at src into dst (which must not exist), copying
// only oci-layout, index.json and the blobs reached from the root, each
// checked against its digest as it is copied, and then checks the copy
// against want. A layout that cannot be read is an error; one that is read
// and refused returns its report and an error wrapping ErrRefused.
func Stage(src, dst, want string) (*Report, error) {
	if !sha256Digest.MatchString(want) {
		return nil, fmt.Errorf("ocilayout: the pin %q is not a sha256 digest", want)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return nil, fmt.Errorf("ocilayout: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "blobs", "sha256"), 0o700); err != nil {
		return nil, fmt.Errorf("ocilayout: %w", err)
	}
	for _, name := range []string{"oci-layout", "index.json"} {
		b, err := readSmall(filepath.Join(src, name))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
			return nil, fmt.Errorf("ocilayout: %w", err)
		}
	}
	w := &walker{copyFrom: src, dir: dst, seen: map[string]bool{}}
	if _, err := w.walk(); err != nil {
		return nil, err
	}
	return Check(dst, want)
}

// Check reads the layout at dir in full and checks it against want.
func Check(dir, want string) (*Report, error) {
	if !sha256Digest.MatchString(want) {
		return nil, fmt.Errorf("ocilayout: the pin %q is not a sha256 digest", want)
	}
	w := &walker{dir: dir, seen: map[string]bool{}}
	r, err := w.walk()
	if err != nil {
		return nil, err
	}
	if r.Manifest != want {
		r.Refusals = append(r.Refusals, fmt.Sprintf("the linux/arm64 manifest is %s, not the pinned %s", r.Manifest, want))
	}
	if len(r.Refusals) > 0 {
		return r, fmt.Errorf("%w: %s", ErrRefused, strings.Join(r.Refusals, "; "))
	}
	return r, nil
}

// walker reads a layout from dir; with copyFrom set, it first copies each
// blob it needs from copyFrom into dir, checking it on the way.
type walker struct {
	dir, copyFrom string
	seen          map[string]bool
	r             Report
}

func (w *walker) walk() (*Report, error) {
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := readJSONFile(filepath.Join(w.dir, "oci-layout"), &layout); err != nil {
		return nil, err
	}
	if layout.Version != "1.0.0" {
		return nil, fmt.Errorf("ocilayout: oci-layout version %q, not 1.0.0", layout.Version)
	}
	var top index
	if err := readJSONFile(filepath.Join(w.dir, "index.json"), &top); err != nil {
		return nil, err
	}
	if len(top.Manifests) != 1 {
		return nil, fmt.Errorf("ocilayout: index.json names %d roots; exactly one is loaded", len(top.Manifests))
	}
	root := top.Manifests[0]
	w.r.Root, w.r.RootMediaType = root.Digest, root.MediaType
	w.r.Reference = root.Annotations["io.containerd.image.name"]
	if w.r.Reference == "" {
		w.r.Reference = root.Annotations["org.opencontainers.image.ref.name"]
	}
	var arm64 *Descriptor
	switch root.MediaType {
	case OCIManifest, DockerManifest:
		arm64 = &root
	case OCIIndex, DockerList:
		var idx index
		if err := w.blobJSON(root, &idx); err != nil {
			return nil, err
		}
		for i := range idx.Manifests {
			d := idx.Manifests[i]
			p := d.Platform
			switch {
			case p != nil && p.OS == "linux" && p.Architecture == "arm64" && (p.Variant == "" || p.Variant == "v8"):
				if arm64 != nil {
					return nil, errors.New("ocilayout: more than one linux/arm64 manifest")
				}
				arm64 = &idx.Manifests[i]
			case p != nil && p.OS == "unknown" && p.Architecture == "unknown":
				// An attestation manifest: it and its blobs are checked (and
				// copied, when staging) like any others, but it is never an
				// image and its layers are not read as a filesystem.
				var am manifest
				if err := w.blobJSON(d, &am); err != nil {
					return nil, err
				}
				for _, b := range append([]Descriptor{am.Config}, am.Layers...) {
					if err := w.blob(b, nil); err != nil {
						return nil, err
					}
				}
			default:
				return nil, fmt.Errorf("ocilayout: a manifest for another platform (%s) is in the layout; only linux/arm64 is loaded", platformString(p))
			}
		}
		if arm64 == nil {
			return nil, errors.New("ocilayout: no linux/arm64 manifest")
		}
	default:
		return nil, fmt.Errorf("ocilayout: the root's media type %q is not an image index or manifest", root.MediaType)
	}
	if err := w.image(*arm64); err != nil {
		return nil, err
	}
	sort.Strings(w.r.CredentialPaths)
	return &w.r, nil
}

func (w *walker) image(d Descriptor) error {
	if d.MediaType != OCIManifest && d.MediaType != DockerManifest {
		return fmt.Errorf("ocilayout: the linux/arm64 entry's media type %q is not an image manifest", d.MediaType)
	}
	w.r.Manifest = d.Digest
	var m manifest
	if err := w.blobJSON(d, &m); err != nil {
		return err
	}
	var c config
	if err := w.blobJSON(m.Config, &c); err != nil {
		return err
	}
	w.r.Config = m.Config.Digest
	w.r.Platform = c.OS + "/" + c.Architecture
	if c.Variant != "" {
		w.r.Platform += "/" + c.Variant
	}
	if c.OS != "linux" || c.Architecture != "arm64" || (c.Variant != "" && c.Variant != "v8") {
		w.r.Refusals = append(w.r.Refusals, "the config's platform is "+w.r.Platform+", not linux/arm64")
	}
	w.env(c.Config.Env)
	if c.RootFS.Type != "layers" || len(c.RootFS.DiffIDs) != len(m.Layers) {
		return fmt.Errorf("ocilayout: the config names %d diff IDs (rootfs type %q) for %d layers", len(c.RootFS.DiffIDs), c.RootFS.Type, len(m.Layers))
	}
	for i, l := range m.Layers {
		var diff string
		err := w.blob(l, func(r io.Reader) error {
			var err error
			diff, err = w.layer(l.MediaType, r)
			return err
		})
		if err != nil {
			return err
		}
		if diff != c.RootFS.DiffIDs[i] {
			w.r.Refusals = append(w.r.Refusals, fmt.Sprintf("layer %d uncompressed is %s, not its diff ID %s", i, diff, c.RootFS.DiffIDs[i]))
		}
		w.r.Layers = append(w.r.Layers, Layer{Digest: l.Digest, MediaType: l.MediaType, Size: l.Size, DiffID: diff})
	}
	return nil
}

// layer reads one layer's tar stream: its uncompressed digest, credential
// files, and os-release (a later layer's copy wins, as it would on unpacking).
func (w *walker) layer(mediaType string, r io.Reader) (string, error) {
	switch mediaType {
	case "application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar":
	case "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip":
		z, err := gzip.NewReader(r)
		if err != nil {
			return "", fmt.Errorf("ocilayout: layer: %w", err)
		}
		defer z.Close()
		r = z
	default:
		return "", fmt.Errorf("ocilayout: layer media type %q is not checked here (uncompressed or gzip only)", mediaType)
	}
	h := sha256.New()
	tr := tar.NewReader(io.TeeReader(r, h))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("ocilayout: layer tar: %w", err)
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if credentialPath(name) {
			w.r.CredentialPaths = append(w.r.CredentialPaths, name)
		}
		if (name == "etc/os-release" || name == "usr/lib/os-release") && hdr.Typeflag == tar.TypeReg && hdr.Size <= 64<<10 {
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", fmt.Errorf("ocilayout: layer tar: %w", err)
			}
			if name == "etc/os-release" || w.r.OSRelease == nil {
				w.r.OSRelease = parseOSRelease(b)
			}
		}
	}
	// Whatever follows the tar's end-of-archive marker is part of the layer too.
	if _, err := io.Copy(io.Discard, io.TeeReader(r, h)); err != nil {
		return "", fmt.Errorf("ocilayout: layer: %w", err)
	}
	if len(w.r.CredentialPaths) > 0 && !contains(w.r.Refusals, credentialRefusal) {
		w.r.Refusals = append(w.r.Refusals, credentialRefusal)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

const credentialRefusal = "a layer holds a credential file (see credential_paths)"

// credentialPath reports whether a layer entry is a file that holds, or may
// hold, a credential. A whiteout for one is not flagged; the file it hides
// is, in the layer that holds its bytes.
func credentialPath(name string) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, ".wh.") {
		return false
	}
	switch base {
	case ".npmrc", ".netrc", ".git-credentials":
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".ssh" {
			return true
		}
	}
	return name == ".docker/config.json" || strings.HasSuffix(name, "/.docker/config.json")
}

var (
	secretName   = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|CREDENTIAL|AUTH)`)
	secretPrefix = regexp.MustCompile(`^(ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|npm_|glpat-|xox[abprs]-|AKIA|ASIA|sk-)`)
)

// env records the config's Env, refusing any variable whose name or value
// looks like a credential. A refused value is not recorded.
func (w *walker) env(vars []string) {
	w.r.Env = []string{}
	for _, kv := range vars {
		name, value, _ := strings.Cut(kv, "=")
		if secretName.MatchString(name) || secretPrefix.MatchString(value) {
			w.r.Env = append(w.r.Env, name+"=<refused>")
			w.r.Refusals = append(w.r.Refusals, "the config's Env has "+name+", which looks like a credential")
			continue
		}
		w.r.Env = append(w.r.Env, kv)
	}
}

// blobJSON reads a small blob whole and decodes it.
func (w *walker) blobJSON(d Descriptor, v any) error {
	if d.Size > maxJSON {
		return fmt.Errorf("ocilayout: %s is %d bytes, over the %d-byte limit for a document", d.Digest, d.Size, maxJSON)
	}
	var b []byte
	err := w.blob(d, func(r io.Reader) error {
		var err error
		b, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("ocilayout: %s: %w", d.Digest, err)
	}
	return nil
}

// blob checks one blob (copying it first, when staging): present, of its
// stated size, and hashing to its name. read, if set, sees its bytes as they
// are hashed; the blob is refused if read fails or leaves bytes unread.
func (w *walker) blob(d Descriptor, read func(io.Reader) error) error {
	if !sha256Digest.MatchString(d.Digest) {
		return fmt.Errorf("ocilayout: digest %q is not sha256", d.Digest)
	}
	rel := filepath.Join("blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
	p := filepath.Join(w.dir, rel)
	if w.copyFrom != "" && !w.seen[d.Digest] {
		if err := copyBlob(filepath.Join(w.copyFrom, rel), p, d); err != nil {
			return err
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("ocilayout: blob %s: %w", d.Digest, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("ocilayout: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() != d.Size {
		return fmt.Errorf("ocilayout: blob %s is %d bytes (regular=%v), not %d", d.Digest, fi.Size(), fi.Mode().IsRegular(), d.Size)
	}
	h := sha256.New()
	tee := io.TeeReader(f, h)
	if read != nil {
		if err := read(tee); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return fmt.Errorf("ocilayout: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return fmt.Errorf("ocilayout: blob %s hashes to %s", d.Digest, got)
	}
	if !w.seen[d.Digest] {
		w.seen[d.Digest] = true
		w.r.Blobs++
		w.r.Bytes += d.Size
	}
	return nil
}

// copyBlob copies one blob, refusing a non-regular source (a symlink out of
// the layout, say) and a copy whose size or hash is not the descriptor's.
func copyBlob(src, dst string, d Descriptor) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("ocilayout: blob %s: %w", d.Digest, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("ocilayout: blob %s is not a regular file", d.Digest)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("ocilayout: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("ocilayout: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, d.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ocilayout: copying %s: %w", d.Digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); n != d.Size || got != d.Digest {
		return fmt.Errorf("ocilayout: blob %s copied as %d bytes hashing to %s", d.Digest, n, got)
	}
	return nil
}

func readSmall(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("ocilayout: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxJSON {
		return nil, fmt.Errorf("ocilayout: %s is not a regular file of at most %d bytes", filepath.Base(p), maxJSON)
	}
	return os.ReadFile(p)
}

func readJSONFile(p string, v any) error {
	b, err := readSmall(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("ocilayout: %s: %w", filepath.Base(p), err)
	}
	return nil
}

func parseOSRelease(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ID", "VERSION_ID", "PRETTY_NAME", "VERSION_CODENAME":
			if ok {
				out[k] = strings.Trim(v, `"'`)
			}
		}
	}
	return out
}

func platformString(p *Platform) string {
	if p == nil {
		return "none stated"
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
