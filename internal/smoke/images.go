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
	Tag    string // the tag it was pinned from, for CheckPinned; never resolved at run time
	Digest string // the index (or manifest) digest the host pulls by
	Arm64  string // the linux/arm64 manifest digest: it keys the base
	// Packages are pinned Debian packages unpacked into the base after the
	// image, or none. They are part of the base's key.
	Packages []debs.Package
}

// Ref is the reference the host pulls: the repository at its pinned digest.
func (g GuestImage) Ref() string { return g.Repo + "@" + g.Digest }

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
