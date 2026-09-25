// Package smoke drives Pomar's first-boot smoke test: fetch the pinned
// kernel, then boot one vsock-only guest from the pinned init filesystem and
// base image, run one command and tear the guest down. Every download and
// object is ledgered in the data root before it is created.
package smoke

import "github.com/zeroemployeeorg/pomar/internal/debs"

// Pinned artifacts. The kernel and the init filesystem are the guest's trust
// roots: the kernel runs the guest, and vminitd is PID 1 inside it with the
// vsock channel to the host. vminit is versioned with Containerization and
// moves only when Containerization does.
const (
	ContainerizationVersion = "0.45.0"

	KernelURL    = "https://github.com/kata-containers/kata-containers/releases/download/3.32.0/kata-static-3.32.0-arm64.tar.zst"
	KernelMember = "opt/kata/share/kata-containers/vmlinux-6.18.35-197-debug"
	KernelName   = "vmlinux-6.18.35-197-debug"

	InitRepo   = "ghcr.io/apple/containerization/vminit"
	InitTag    = "0.45.0"
	InitDigest = "sha256:aa6ab59d0938f7fadb54ac27e80959bdd2f1dafa8050011086d5f8ab1350fd6c"

	ImageRepo   = "docker.io/library/golang"
	ImageTag    = "1.27.0-bookworm"
	ImageDigest = "sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452"
	// ImageArm64 keys the base root filesystem built from the image.
	ImageArm64 = "sha256:4220c5d84f685eb34a728d389bab47674b46f433b5218ae9a75a5fbd5e0be724"
	// BaseSizeBytes is the ext4 capacity of a base (and so of each clone).
	// The ext4 image is sparse and each clone is copy-on-write, so the host
	// pays only for what is written; the capacity bounds what one guest can
	// write. 2 GiB was too small for a Go gate with -race (POMAR-SOW-03 §36).
	BaseSizeBytes = 16 << 30
)

// CIPackages are the Debian packages the CI class's base carries beyond its
// image (elders' ruling r12 §3): jq and the libraries it links, from the
// bookworm arm64 archive, each pinned to the sha256 its Packages index lists.
// A changed list is a new base. Each addition is named in a SOW.
var CIPackages = []debs.Package{
	{Name: "jq", URL: "https://deb.debian.org/debian/pool/main/j/jq/jq_1.6-2.1+deb12u2_arm64.deb",
		SHA256: "c232e9407e0f47006dd6077804c1274fd2e4f8be02efc78822db748ed65bea99"},
	{Name: "libjq1", URL: "https://deb.debian.org/debian/pool/main/j/jq/libjq1_1.6-2.1+deb12u2_arm64.deb",
		SHA256: "2f5b9f70fd6d954123c3c8d0a1b306fe3703dc88c6362ccbe28713e55e7a730b"},
	{Name: "libonig5", URL: "https://deb.debian.org/debian/pool/main/libo/libonig/libonig5_6.9.8-1_arm64.deb",
		SHA256: "4693ac0f4fc8f2b8b4a7854463a8061a5b0fe7a7c2eb46b9cc21bc4c3adaf1ee"},
}
