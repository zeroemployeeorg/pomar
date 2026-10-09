package smoke

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/debs"
)

// GuestImage is one pinned image a class can boot, and so the base it clones.
// The catalogue is code, not configuration: a class chooses an image by name
// (-class-image), never by reference, so a new image is a reviewed change
// here, pinned by its index digest and its arm64 manifest digest.
type GuestImage struct {
	Name   string // what a class names, like "golang-ci"
	Repo   string // the registry repository
	Tag    string // original remote tag, or local store tag guarded by Digest
	Digest string // the index (or manifest) digest the host pulls by
	Arm64  string // the linux/arm64 manifest digest: it keys the base
	// LocalLayout selects a checked, locally loaded tag. The helper still
	// requires its index digest to equal Digest before unpacking or booting.
	LocalLayout bool
	// Packages are pinned Debian packages unpacked into the base after the
	// image, or none. They are part of the base's key.
	Packages []debs.Package
}

// Ref is the store lookup: remote images use their digest, local layouts
// use their reviewed tag. Both paths require the pinned digest in the helper.
func (g GuestImage) Ref() string {
	if g.LocalLayout {
		return g.Repo + ":" + g.Tag
	}
	return g.Repo + "@" + g.Digest
}

// PackageSet is the hash of the image's packages (debs.SetHash), or "".
func (g GuestImage) PackageSet() string {
	if len(g.Packages) == 0 {
		return ""
	}
	return debs.SetHash(g.Packages)
}

// DefaultGuestImage is the CI class's image, the default for every class.
const DefaultGuestImage = "golang-ci"

// GuestImages is the catalogue. The first is the CI image the smoke test and
// the CI class have always used.
var GuestImages = []GuestImage{
	{Name: DefaultGuestImage, Repo: ImageRepo, Tag: ImageTag, Digest: ImageDigest, Arm64: ImageArm64, Packages: CIPackages},
	// Node 24.21.0, the Active LTS, for npm classes and their guest proof.
	// Slim: no compiler or headers, so it does not establish native-addon
	// readiness. The index, the arm64 manifest and its config were each
	// hashed and matched their digests on 2026-10-02 (POMAR-SOW-04).
	{Name: "node24-slim", Repo: "docker.io/library/node", Tag: "24-bookworm-slim",
		Digest: "sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6",
		Arm64:  "sha256:24b8bc17702002d2ed0c1da9ad66c3ee507cc279d0856726662ff2b6fc35c149"},
	// The same Node 24.21.0 on full bookworm, which carries a compiler, make,
	// python3 and Node's headers, for classes that build native addons. The
	// toolchain in the image is not readiness: that is a guest check that
	// builds and loads a locked addon offline. The index, the arm64 manifest
	// and its config were each hashed and matched their digests on 2026-10-03.
	{Name: "node24-full", Repo: "docker.io/library/node", Tag: "24-bookworm",
		Digest: "sha256:64af3819f9275802414d7cdc38c27e9d82bd564dec4d4da87d008255d36c63b4",
		Arm64:  "sha256:91882e0e5959240d4413fc42c180022bbdd09c5491e00e75faa6c100d8d7751b"},
	// Guest-qualified local OCI layout; see docs/runner-tools-image.md.
	// This is a toolchain pin, not a project CI or release qualification.
	{Name: "node24-python314-chromium", Repo: "localhost/pomar/runner-tools", Tag: "20261007-v5", LocalLayout: true,
		Digest: "sha256:498c46febe123361a398475cdf47da02426ee1b6d6982ca15f190ac5bcc320bd",
		Arm64:  "sha256:ab317bddf37bbc3bc03e5f51b40636512b9e2ef1266983791e8e439e24a4d7ae"},
	// The same runner tools with tmux, for interactive seats (POMAR-CC SOW 15
	// §5): its base is the runner-tools layout plus SeatPackages. This names
	// the image only; it qualifies no seat, agent version or class.
	{Name: "seat-runner-tools", Repo: "localhost/pomar/runner-tools", Tag: "20261007-v5", LocalLayout: true,
		Digest:   "sha256:498c46febe123361a398475cdf47da02426ee1b6d6982ca15f190ac5bcc320bd",
		Arm64:    "sha256:ab317bddf37bbc3bc03e5f51b40636512b9e2ef1266983791e8e439e24a4d7ae",
		Packages: SeatPackages},
	// The CI image with tmux, for qualifying Claude Code in an interactive
	// seat (POMAR-CC SOW 15 §5.4) on a development host that holds no
	// runner-tools layout. It is a qualification base only: no seat runs
	// its work on it.
	{Name: "seat-qualification", Repo: ImageRepo, Tag: ImageTag, Digest: ImageDigest, Arm64: ImageArm64,
		Packages: append(append([]debs.Package{}, CIPackages...), SeatPackages...)},
}

var (
	imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	sha256RE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// GuestImageByName returns the catalogue's image name, or an error naming
// the images there are.
func GuestImageByName(name string) (GuestImage, error) {
	var names []string
	for _, g := range GuestImages {
		if g.Name == name {
			return g, nil
		}
		names = append(names, g.Name)
	}
	return GuestImage{}, fmt.Errorf("no pinned guest image %q; the catalogue holds %s", name, strings.Join(names, ", "))
}

// checkGuestImages is the catalogue's own consistency: distinct plain names,
// full digests, and distinct bases.
func checkGuestImages(images []GuestImage) error {
	names, bases := map[string]bool{}, map[string]bool{}
	for _, g := range images {
		if !imageNameRE.MatchString(g.Name) || names[g.Name] {
			return fmt.Errorf("guest image name %q is not plain or not distinct", g.Name)
		}
		names[g.Name] = true
		if g.Repo == "" || g.Tag == "" || !sha256RE.MatchString(g.Digest) || !sha256RE.MatchString(g.Arm64) {
			return fmt.Errorf("guest image %q needs a repository, a tag and two full sha256 digests", g.Name)
		}
		key := g.Arm64 + "/" + g.PackageSet()
		if bases[key] {
			return fmt.Errorf("guest image %q has the same base as another", g.Name)
		}
		bases[key] = true
	}
	return nil
}
