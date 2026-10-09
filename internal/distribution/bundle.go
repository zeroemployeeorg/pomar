// Package distribution verifies complete, versioned Pomar distributions. It
// manages only an explicit user installation prefix, never services or data.
package distribution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const Schema = "pomar.release/v1"

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9]+(?:[.-][a-z0-9]+)*)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var Files = []string{"bin/pomar", "bin/pomar-host", "bin/pomar-shim-linux-arm64", "bin/pomar-agent-owner", "bin/pomar-ci-caller", "LICENSE", "README.md", "CONTRIBUTING.md", "docs/install.md", "docs/operations.md", "docs/releases.md", "docs/normal-clients.md", "docs/manager-classes.md", "docs/agent-environments.md"}

type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}
type Manifest struct {
	Schema       string          `json:"schema"`
	Version      string          `json:"version"`
	Commit       string          `json:"source_commit"`
	Platform     string          `json:"platform"`
	MinimumMacOS string          `json:"minimum_macos"`
	Files        map[string]File `json:"files"`
}

func ValidIdentity(version, commit string) bool {
	return versionPattern.MatchString(version) && commitPattern.MatchString(commit)
}
func HashFile(path string) (File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return File{}, err
	}
	if !st.Mode().IsRegular() {
		return File{}, fmt.Errorf("not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if !os.SameFile(st, opened) {
		return File{}, fmt.Errorf("file changed: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	if n != st.Size() {
		return File{}, fmt.Errorf("size changed: %s", path)
	}
	return File{hex.EncodeToString(h.Sum(nil)), n, uint32(st.Mode().Perm())}, nil
}
func WriteManifest(root, version, commit string) (Manifest, error) {
	m := Manifest{Schema, version, commit, "darwin-arm64", "26", map[string]File{}}
	if !ValidIdentity(version, commit) {
		return m, fmt.Errorf("use an explicit vMAJOR.MINOR.PATCH[-prerelease] and full source commit")
	}
	for _, name := range Files {
		f, err := HashFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return m, err
		}
		m.Files[name] = f
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	err = os.WriteFile(filepath.Join(root, "release.json"), append(b, '\n'), 0644)
	return m, err
}
func Verify(root string) (Manifest, error) {
	var m Manifest
	st, err := os.Lstat(root)
	if err != nil {
		return m, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return m, fmt.Errorf("bundle must be a real directory")
	}
	mf, err := HashFile(filepath.Join(root, "release.json"))
	if err != nil {
		return m, err
	}
	if mf.Size > 64<<10 {
		return m, fmt.Errorf("oversized release manifest")
	}
	raw, err := os.ReadFile(filepath.Join(root, "release.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Schema != Schema || m.Platform != "darwin-arm64" || m.MinimumMacOS != "26" || !ValidIdentity(m.Version, m.Commit) || len(m.Files) != len(Files) {
		return m, fmt.Errorf("unsupported or incomplete release manifest")
	}
	allowed := map[string]bool{"release.json": true}
	for _, n := range Files {
		allowed[n] = true
	}
	dirs := map[string]bool{".": true, "bin": true, "docs": true}
	err = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		r = filepath.ToSlash(r)
		if e.IsDir() {
			if !dirs[r] {
				return fmt.Errorf("unexpected directory %s", r)
			}
			return nil
		}
		if !allowed[r] || !e.Type().IsRegular() {
			return fmt.Errorf("unexpected or non-regular release entry %s", r)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	for _, name := range Files {
		want, ok := m.Files[name]
		if !ok {
			return m, fmt.Errorf("missing manifest entry %s", name)
		}
		mode := uint32(0644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0755
		}
		if want.Mode != mode {
			return m, fmt.Errorf("unsafe mode for %s", name)
		}
		got, err := HashFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return m, err
		}
		if got != want {
			return m, fmt.Errorf("release file mismatch: %s", name)
		}
	}
	return m, nil
}

// Install copies verified bytes into a staging directory, re-verifies them, and
// publishes a version without overwriting an existing one. current is replaced
// atomically only after a complete version exists. A failed activation leaves
// that recoverable version and the previous selection intact.
func Install(bundle, prefix string) (Manifest, error) {
	m, err := Verify(bundle)
	if err != nil {
		return m, err
	}
	prefix = filepath.Clean(prefix)
	if !filepath.IsAbs(prefix) || prefix == string(os.PathSeparator) {
		return m, fmt.Errorf("prefix must be an absolute user-owned directory")
	}
	if err = secureDirectory(prefix); err != nil {
		return m, err
	}
	releases := filepath.Join(prefix, "releases")
	if err = secureDirectory(releases); err != nil {
		return m, err
	}
	lock := filepath.Join(prefix, ".install-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return m, fmt.Errorf("installation locked (inspect an interrupted installer before removing its lock): %w", err)
	}
	defer os.Remove(lock)
	current := filepath.Join(prefix, "current")
	if st, e := os.Lstat(current); e == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return m, fmt.Errorf("current is not a Pomar version link")
		}
		target, e := os.Readlink(current)
		if e != nil || !strings.HasPrefix(target, "releases/") || strings.Contains(strings.TrimPrefix(target, "releases/"), "/") {
			return m, fmt.Errorf("current is not a Pomar version link")
		}
	} else if !os.IsNotExist(e) {
		return m, e
	}
	dest := filepath.Join(releases, m.Version)
	if _, e := os.Lstat(dest); e == nil {
		old, e := Verify(dest)
		if e != nil {
			return m, e
		}
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(m)
		if string(a) != string(b) {
			return m, fmt.Errorf("version %s already has different bytes; never overwrite a release", m.Version)
		}
	} else if !os.IsNotExist(e) {
		return m, e
	} else {
		stage, e := os.MkdirTemp(releases, ".stage-")
		if e != nil {
			return m, e
		}
		defer os.RemoveAll(stage)
		for _, dir := range []string{"bin", "docs"} {
			if e = os.Mkdir(filepath.Join(stage, dir), 0755); e != nil {
				return m, e
			}
		}
		names := append(append([]string{}, Files...), "release.json")
		sort.Strings(names)
		for _, name := range names {
			src, e := os.Open(filepath.Join(bundle, filepath.FromSlash(name)))
			if e != nil {
				return m, e
			}
			mode := os.FileMode(0644)
			if strings.HasPrefix(name, "bin/") {
				mode = 0755
			}
			dst, e := os.OpenFile(filepath.Join(stage, filepath.FromSlash(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				src.Close()
				return m, e
			}
			_, e = io.Copy(dst, src)
			src.Close()
			syncErr := dst.Sync()
			closeErr := dst.Close()
			if e != nil {
				return m, e
			}
			if syncErr != nil {
				return m, syncErr
			}
			if closeErr != nil {
				return m, closeErr
			}
			if e = os.Chmod(filepath.Join(stage, filepath.FromSlash(name)), mode); e != nil {
				return m, e
			}
		}
		checked, e := Verify(stage)
		if e != nil {
			return m, e
		}
		a, _ := json.Marshal(checked)
		b, _ := json.Marshal(m)
		if string(a) != string(b) {
			return m, fmt.Errorf("bundle changed during copy")
		}
		if e = os.Chmod(stage, 0755); e != nil {
			return m, e
		}
		if e = os.Rename(stage, dest); e != nil {
			return m, e
		}
	}
	tmp, err := os.MkdirTemp(prefix, ".activate-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)
	link := filepath.Join(tmp, "current")
	if err = os.Symlink(filepath.Join("releases", m.Version), link); err != nil {
		return m, err
	}
	err = os.Rename(link, current)
	return m, err
}

// Existing prefix components must belong to this user, be directories, and be
// non-writable by others. Do not follow symlinks through an installation root.
func secureDirectory(path string) error {
	missing := []string{}
	for p := path; p != filepath.Dir(p); p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			missing = append(missing, p)
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink/non-directory in prefix: %s", p)
		}
		if st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("writable installation parent: %s", p)
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0755); err != nil {
			return err
		}
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return ownedByCaller(st)
}
