// pomar-dist packages already built, signed release executables. It never
// fetches source, runs a service, creates a tag or publishes a release.
package main

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/zeroemployeeorg/pomar/internal/distribution"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pomar-dist:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	fs := flag.NewFlagSet("pomar-dist", flag.ContinueOnError)
	version := fs.String("version", "", "explicit vMAJOR.MINOR.PATCH[-prerelease]")
	commit := fs.String("commit", "", "full clean source commit")
	bins := fs.String("bin-dir", "", "directory of signed darwin/arm64 tools and static linux/arm64 shim")
	source := fs.String("source", ".", "source containing public docs/LICENSE")
	out := fs.String("out", "", "new output directory")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if !distribution.ValidIdentity(*version, *commit) || *bins == "" || *out == "" || fs.NArg() != 0 {
		return fmt.Errorf("required: -version -commit -bin-dir -source -out")
	}
	// Validate target formats before creating output. The build script performs
	// host entitlement signing and verifies clean VCS stamping separately.
	for _, name := range distribution.Files {
		if !strings.HasPrefix(name, "bin/") {
			continue
		}
		p := filepath.Join(*bins, filepath.Base(name))
		if strings.HasSuffix(name, "linux-arm64") {
			f, err := elf.Open(p)
			if err != nil {
				return err
			}
			if f.Machine != elf.EM_AARCH64 || f.Type != elf.ET_EXEC {
				f.Close()
				return fmt.Errorf("shim must be linux/arm64 ET_EXEC")
			}
			for _, seg := range f.Progs {
				if seg.Type == elf.PT_INTERP {
					f.Close()
					return fmt.Errorf("shim must be static")
				}
			}
			f.Close()
		} else {
			f, err := macho.Open(p)
			if err != nil {
				return err
			}
			ok := f.Cpu == macho.CpuArm64 && f.Type == macho.TypeExec
			f.Close()
			if !ok {
				return fmt.Errorf("%s must be darwin/arm64 executable", name)
			}
		}
	}
	if err := os.Mkdir(*out, 0755); err != nil {
		return err
	}
	rootName := "pomar-" + *version + "-darwin-arm64"
	root := filepath.Join(*out, rootName)
	if err := os.Mkdir(root, 0755); err != nil {
		return err
	}
	for _, d := range []string{"bin", "docs"} {
		if err := os.Mkdir(filepath.Join(root, d), 0755); err != nil {
			return err
		}
	}
	for _, name := range distribution.Files {
		src := filepath.Join(*source, filepath.FromSlash(name))
		mode := os.FileMode(0644)
		if strings.HasPrefix(name, "bin/") {
			src = filepath.Join(*bins, filepath.Base(name))
			mode = 0755
		}
		if err := copyFile(src, filepath.Join(root, filepath.FromSlash(name)), mode); err != nil {
			return err
		}
	}
	m, err := distribution.WriteManifest(root, *version, *commit)
	if err != nil {
		return err
	}
	if _, err = distribution.Verify(root); err != nil {
		return err
	}
	archive := filepath.Join(*out, rootName+".tar.gz")
	if err = pack(root, rootName, archive); err != nil {
		return err
	}
	hash, err := distribution.HashFile(archive)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*out, "SHA256SUMS"), []byte(hash.SHA256+"  "+filepath.Base(archive)+"\n"), 0644); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err = os.WriteFile(filepath.Join(*out, "release.json"), append(b, '\n'), 0644); err != nil {
		return err
	}
	// A usable installer is a separate, pinned asset, not a mutable script pipe.
	if err = copyFile(filepath.Join(*source, "tools", "install.sh"), filepath.Join(*out, "install.sh"), 0755); err != nil {
		return err
	}
	inst, err := distribution.HashFile(filepath.Join(*out, "install.sh"))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(*out, "SHA256SUMS"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s  install.sh\n", inst.SHA256)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func copyFile(src, dst string, mode os.FileMode) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file: %s", src)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	o, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(o, f)
	if err == nil {
		err = o.Chmod(mode)
	}
	ce := o.Close()
	if err != nil {
		return err
	}
	return ce
}

// Stable metadata and ordering make identical verified files produce the same
// archive. No symlinks, cache paths, source checkout or private records enter it.
func pack(root, name, out string) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	names := append(append([]string{}, distribution.Files...), "release.json")
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: name + "/" + n, Mode: int64(st.Mode().Perm()), Size: st.Size(), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, r)
		r.Close()
		if err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	return f.Sync()
}
