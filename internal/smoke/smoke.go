package smoke

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Paths inside the data root. downloads/ and kernels/ are venue structure.
const (
	downloadsDir = "downloads"
	kernelsDir   = "kernels"
	storeDir     = "store"
	tmpDir       = "tmp"
)

// Env carries what both steps need.
type Env struct {
	Venue   *venue.Venue
	HostBin string // the signed pomar-host binary
	Client  *http.Client
	Out     io.Writer
}

func (e *Env) logf(format string, args ...any) { fmt.Fprintf(e.Out, format+"\n", args...) }

// KernelPath is where FetchKernel leaves the extracted kernel.
func (e *Env) KernelPath() string {
	return filepath.Join(e.Venue.Root(), kernelsDir, KernelName)
}

// FetchKernel downloads the pinned kernel tarball, reports its sha256,
// extracts the kernel and removes the tarball. The extracted kernel stays
// ledgered and open for reuse.
func FetchKernel(ctx context.Context, e *Env) error {
	v := e.Venue
	if err := v.Init(); err != nil {
		return err
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		return err
	}
	tarRel := filepath.Join(downloadsDir, filepath.Base(KernelURL))
	if err := v.Intent(venue.KindDownload, venue.ClassAttempt, "kernel-tarball", tarRel, KernelURL); err != nil {
		return err
	}
	tarAbs := filepath.Join(v.Root(), tarRel)
	sum, n, err := download(ctx, e.Client, KernelURL, tarAbs)
	if err != nil {
		v.Failed(venue.KindDownload, "kernel-tarball", err.Error())
		return err
	}
	if err := v.Created(venue.KindDownload, "kernel-tarball"); err != nil {
		return err
	}
	e.logf("kernel_tarball_url=%s", KernelURL)
	e.logf("kernel_tarball_bytes=%d", n)
	e.logf("kernel_tarball_sha256=%s", sum)

	kRel := filepath.Join(kernelsDir, KernelName)
	if err := v.Intent(venue.KindImage, venue.ClassCache, "kernel", kRel, "extracted from sha256:"+sum); err != nil {
		return err
	}
	out, err := e.host(ctx, "extract-kernel", "--archive", tarAbs, "--member", KernelMember, "--out", e.KernelPath())
	e.logf("%s", strings.TrimSpace(out))
	if err != nil {
		v.Failed(venue.KindImage, "kernel", err.Error())
		return err
	}
	ksum, err := fileSHA256(e.KernelPath())
	if err != nil {
		return err
	}
	if err := v.Created(venue.KindImage, "kernel"); err != nil {
		return err
	}
	e.logf("kernel_sha256=%s", ksum)
	// The tarball has served its purpose; the kernel is what is pinned.
	if err := v.Teardown(venue.KindDownload, "kernel-tarball"); err != nil {
		return err
	}
	e.logf("kernel_tarball=removed")
	return nil
}

// Boot checks the pins against the registries, then boots one guest.
// kernelSHA256 must match the extracted kernel. The image store stays
// ledgered and open as the host-side cache; the guest is torn down.
func Boot(ctx context.Context, e *Env, kernelSHA256, id string) error {
	v := e.Venue
	if err := v.Init(); err != nil {
		return err
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		return err
	}
	got, err := fileSHA256(e.KernelPath())
	if err != nil {
		return err
	}
	if got != kernelSHA256 {
		return fmt.Errorf("smoke: kernel sha256 %s, pinned %s", got, kernelSHA256)
	}
	e.logf("kernel_sha256=%s (matches pin)", got)
	for _, p := range [][3]string{{InitRepo, InitTag, InitDigest}, {ImageRepo, ImageTag, ImageDigest}} {
		if err := CheckPinned(ctx, e.Client, p[0], p[1], p[2]); err != nil {
			return err
		}
		e.logf("tag_check %s:%s -> %s (matches pin)", p[0], p[1], p[2])
	}

	if err := v.EnsureCache(venue.KindImage, "image-store", storeDir); err != nil {
		return err
	}
	if err := v.EnsureCache(venue.KindVolume, "tmp", tmpDir); err != nil {
		return err
	}

	vmRel := filepath.Join(storeDir, "containers", id)
	if err := v.Intent(venue.KindVM, venue.ClassAttempt, id, vmRel, "first-boot smoke, vsock only"); err != nil {
		return err
	}
	out, runErr := e.host(ctx, "boot-smoke",
		"--store", filepath.Join(v.Root(), storeDir),
		"--kernel", e.KernelPath(),
		"--init", InitRepo+"@"+InitDigest, "--init-digest", InitDigest,
		"--image", ImageRepo+"@"+ImageDigest, "--image-digest", ImageDigest,
		"--id", id)
	fmt.Fprint(e.Out, out)
	if runErr != nil {
		v.Failed(venue.KindVM, id, runErr.Error())
	} else if err := v.Created(venue.KindVM, id); err != nil {
		return err
	}
	// The guest is torn down whether or not it ran.
	if err := v.Teardown(venue.KindVM, id); err != nil {
		return err
	}
	e.logf("vm %s: torn down", id)
	return runErr
}

// host runs pomar-host with TMPDIR inside the data root, so that nothing it
// unpacks lands elsewhere. The temp directory is ledgered before first use.
func (e *Env) host(ctx context.Context, args ...string) (string, error) {
	if err := e.Venue.EnsureCache(venue.KindVolume, "tmp", tmpDir); err != nil {
		return "", err
	}
	tmp := filepath.Join(e.Venue.Root(), tmpDir)
	cmd := exec.CommandContext(ctx, e.HostBin, args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp+string(filepath.Separator))
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func download(ctx context.Context, client *http.Client, u, dst string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("smoke: GET %s: %s", u, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), bufio.NewReaderSize(resp.Body, 1<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewClient returns an HTTP client with a generous overall timeout.
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Minute} }

// VerifyKernel checks the extracted kernel against its pinned sha256.
func VerifyKernel(e *Env, pinned string) error {
	got, err := fileSHA256(e.KernelPath())
	if err != nil {
		return err
	}
	if got != pinned {
		return fmt.Errorf("smoke: kernel sha256 %s, pinned %s", got, pinned)
	}
	return nil
}
