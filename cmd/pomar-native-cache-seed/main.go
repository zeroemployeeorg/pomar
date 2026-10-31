// Command pomar-native-cache-seed is an owner-operated, one-use preparation
// helper. It imports authenticated static inputs into a NEW venue through the
// maintained ledger API. It never builds a base, marks a boot, or starts a VM.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const maxManifest = 64 << 10

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type inputFile struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Schema string      `json:"schema"`
	Files  []inputFile `json:"files"`
}

type cache struct {
	kind     venue.Kind
	id, path string
	files    []inputFile
}

type testHooks struct {
	afterIntent func(cache) error
	diskGuard   func(*venue.Venue) error
}

func main() {
	fs := flag.NewFlagSet("pomar-native-cache-seed", flag.ContinueOnError)
	root := fs.String("root", "", "NEW absolute private venue; existing roots are refused, including an uncertain prior attempt")
	inputs := fs.String("inputs", "", "absolute owner-received input directory, without symlink parents")
	packet := fs.String("manifest", "", "bounded input manifest; obtain its expected digest independently")
	pin := fs.String("manifest-sha256", "", "independently retained exact manifest SHA256")
	if fs.Parse(os.Args[1:]) != nil || fs.NArg() != 0 || *root == "" || *inputs == "" || *packet == "" || !digest.MatchString(*pin) {
		os.Exit(2)
	}
	if err := prepare(*root, *inputs, *packet, *pin, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(`{"stage":"static_cache_inputs_created","base_built":false,"boot_verified":false,"vm_started":false}`)
}

func local(name string) bool {
	return filepath.IsLocal(name) && filepath.ToSlash(filepath.Clean(name)) == name && !strings.Contains(name, `\`)
}

func checkedDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("directory must be absolute and canonical")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != path {
		return errors.New("directory or parent is absent or redirected")
	}
	s, err := os.Stat(path)
	if err != nil || !s.IsDir() || s.Mode().Perm()&0022 != 0 {
		return errors.New("directory must be owner controlled")
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errors.New("directory must belong to the executing owner")
	}
	return nil
}

func decode(raw []byte, pin string) ([]cache, error) {
	sum := sha256.Sum256(raw)
	if !digest.MatchString(pin) || hex.EncodeToString(sum[:]) != pin {
		return nil, errors.New("input manifest does not match independently retained pin")
	}
	var m manifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF || m.Schema != "pomar.native-cache-seed/v1" || len(m.Files) < 4 || len(m.Files) > 64 {
		return nil, errors.New("invalid bounded native cache manifest")
	}
	groups := []cache{{kind: venue.KindImage, id: "image-store", path: "store"}, {kind: venue.KindDownload, id: "debs", path: "downloads/debs"}, {kind: venue.KindImage, id: "kernel", path: "kernels/" + smoke.KernelName}}
	seen := map[string]bool{}
	var total int64
	state, blob := false, false
	for _, f := range m.Files {
		if !local(f.Source) || !local(f.Target) || !digest.MatchString(f.SHA256) || f.Bytes < 1 || f.Bytes > 256<<20 || seen[f.Target] {
			return nil, errors.New("invalid input path, size, digest or duplicate target")
		}
		seen[f.Target] = true
		total += f.Bytes
		if total > 1<<30 {
			return nil, errors.New("static inputs exceed one GiB")
		}
		group := -1
		switch {
		case f.Target == "store/state.json":
			group, state = 0, true
		case f.Target == "store/content/blobs/sha256/"+f.SHA256:
			group, blob = 0, true
		case f.Target == "downloads/debs/"+f.SHA256+".deb":
			group = 1
		case f.Target == groups[2].path:
			group = 2
		}
		if group < 0 {
			return nil, errors.New("target is outside the fixed store/deb/kernel cache recipe")
		}
		groups[group].files = append(groups[group].files, f)
	}
	if !state || !blob || len(groups[1].files) == 0 || len(groups[2].files) != 1 {
		return nil, errors.New("incomplete static cache recipe")
	}
	return groups, nil
}

// openInput rejects leaf and intermediate symlinks; OpenRoot additionally
// confines path resolution during a concurrent directory change.
func openInput(root *os.Root, name string) (*os.File, error) {
	parts := strings.Split(name, "/")
	for i := range parts {
		s, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || s.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("input path is absent or redirected")
		}
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("input must be a regular file")
	}
	return f, nil
}

func verify(f *os.File, pin inputFile, output io.Writer) error {
	before, err := f.Stat()
	if err != nil || before.Size() != pin.Bytes {
		return errors.New("input size mismatch")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, output), io.LimitReader(f, pin.Bytes+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || n != pin.Bytes || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || hex.EncodeToString(h.Sum(nil)) != pin.SHA256 {
		return errors.New("input changed or does not match its pin")
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Tests can observe intent ordering and supply deterministic capacity. The
// executable exposes no hook or disk-limit override; production passes nil.
func prepare(root, inputs, packet, pin string, hooks *testHooks) error {
	if os.Getuid() == 0 {
		return errors.New("use the existing unprivileged owner")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("new venue must be an absolute canonical path")
	}
	if err := checkedDirectory(filepath.Dir(root)); err != nil {
		return err
	}
	if err := checkedDirectory(inputs); err != nil {
		return err
	}
	f, err := os.OpenFile(packet, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManifest {
		f.Close()
		return errors.New("manifest must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(raw) > maxManifest {
		return errors.New("manifest read failed or exceeded limit")
	}
	groups, err := decode(raw, pin)
	if err != nil {
		return err
	}
	src, err := os.OpenRoot(inputs)
	if err != nil {
		return err
	}
	defer src.Close()
	// No target effects occur until ALL source payloads pass verification.
	for _, group := range groups {
		for _, file := range group.files {
			f, err := openInput(src, file.Source)
			if err != nil {
				return err
			}
			err = verify(f, file, io.Discard)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return fmt.Errorf("new venue refused; preserve and inspect any previous attempt: %w", err)
	}
	if err := syncDirectory(filepath.Dir(root)); err != nil {
		return err
	}
	v, err := venue.Open(root)
	if err != nil {
		return err
	}
	if err := v.Init(); err != nil {
		return err
	}
	dst, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, group := range groups {
		guard := func(v *venue.Venue) error { return v.CheckHeavy(venue.DefaultMaxFillPercent) }
		if hooks != nil && hooks.diskGuard != nil {
			guard = hooks.diskGuard
		}
		if err := guard(v); err != nil {
			return err
		}
		if err := v.Intent(group.kind, venue.ClassCache, group.id, group.path, "independently pinned static native inputs; no build or boot claim"); err != nil {
			return err
		}
		if err := syncDirectory(root); err != nil {
			return err
		}
		err := func() error {
			if hooks != nil && hooks.afterIntent != nil {
				if err := hooks.afterIntent(group); err != nil {
					return err
				}
			}
			for _, file := range group.files {
				if err := dst.MkdirAll(filepath.Dir(file.Target), 0700); err != nil {
					return err
				}
				in, err := openInput(src, file.Source)
				if err != nil {
					return err
				}
				out, err := dst.OpenFile(file.Target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
				if err != nil {
					in.Close()
					return err
				}
				err = verify(in, file, out)
				in.Close()
				if err == nil {
					err = out.Chmod(0444)
				}
				if err == nil {
					err = out.Sync()
				}
				closeErr := out.Close()
				if err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
				readback, err := dst.Open(file.Target)
				if err != nil {
					return err
				}
				err = verify(readback, file, io.Discard)
				readback.Close()
				if err != nil {
					return err
				}
				for path := filepath.Dir(filepath.Join(root, file.Target)); ; path = filepath.Dir(path) {
					if err := syncDirectory(path); err != nil {
						return err
					}
					if path == root {
						break
					}
				}
			}
			return nil
		}()
		if err != nil {
			return errors.Join(err, v.Failed(group.kind, group.id, err.Error()))
		}
		if err := v.Created(group.kind, group.id); err != nil {
			return err
		}
	}
	return nil
}
