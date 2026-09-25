// Package debs supplies a job class's pinned Debian packages to its base
// root filesystem. The host fetches each .deb once from its archive URL,
// checks it against its pinned sha256, and extracts its data archive, which
// the base build unpacks after the image's layers. Guests gain no network
// path: packages arrive only inside the base.
//
// It is not a package manager. There is no dependency resolution and no
// maintainer script runs: each class names every package it needs, each
// addition is named in a SOW, and a changed list means a new base.
package debs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Package is one pinned .deb.
type Package struct {
	Name   string
	URL    string // https, on the Debian archive
	SHA256 string // of the .deb, as the archive's Packages index lists it
}

// Dir is the data-root path of the package cache.
const Dir = "downloads/debs"

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SetHash identifies a package set: the sha256 of its sorted .deb hashes.
// A base built with the set is keyed by it.
func SetHash(pkgs []Package) string {
	sums := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		sums = append(sums, p.SHA256)
	}
	sort.Strings(sums)
	h := sha256.Sum256([]byte(strings.Join(sums, "\n")))
	return hex.EncodeToString(h[:])
}

// Cache fetches and unpacks pinned packages into the data root.
type Cache struct {
	Venue  *venue.Venue
	Client *http.Client // nil: a client with a timeout
}

// DataArchives returns, in the order given, the path of each package's
// extracted data archive (data.tar.xz), fetching and checking any package
// not yet cached. The cache is a ledgered cache object, created before its
// first file.
func (c *Cache) DataArchives(ctx context.Context, pkgs []Package) ([]string, error) {
	v := c.Venue
	if err := v.EnsureCache(venue.KindDownload, "debs", Dir); err != nil {
		return nil, err
	}
	dir := filepath.Join(v.Root(), Dir)
	var out []string
	for _, p := range pkgs {
		if !hexSHA.MatchString(p.SHA256) || !strings.HasPrefix(p.URL, "https://") {
			return nil, fmt.Errorf("debs: %s: invalid pin", p.Name)
		}
		deb := filepath.Join(dir, p.SHA256+".deb")
		if err := c.ensure(ctx, p, deb); err != nil {
			return nil, err
		}
		data := filepath.Join(dir, p.SHA256+".data.tar.xz")
		if _, err := os.Stat(data); err != nil {
			if err := extractData(deb, data); err != nil {
				return nil, fmt.Errorf("debs: %s: %w", p.Name, err)
			}
		}
		out = append(out, data)
	}
	return out, nil
}

// ensure puts the pinned .deb at path, verified; a cached file is
// re-verified on every use.
func (c *Cache) ensure(ctx context.Context, p Package, path string) error {
	if sum, err := fileSHA256(path); err == nil {
		if sum == p.SHA256 {
			return nil
		}
		os.Remove(path)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("debs: %s: %w", p.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("debs: %s: %s", p.Name, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".part-*")
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 64<<20)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != p.SHA256 {
		os.Remove(tmp.Name())
		return fmt.Errorf("debs: %s: sha256 %s does not match the pin %s", p.Name, got, p.SHA256)
	}
	return os.Rename(tmp.Name(), path)
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

// extractData copies the data.tar.xz member of a .deb (an ar archive) to
// dst. Only an xz-compressed data member is accepted.
func extractData(deb, dst string) error {
	b, err := os.ReadFile(deb)
	if err != nil {
		return err
	}
	data, err := arMember(b, "data.tar.xz")
	if err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// arMember returns one member of an ar archive by name.
func arMember(b []byte, name string) ([]byte, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(b, []byte(magic)) {
		return nil, errors.New("not an ar archive")
	}
	off := len(magic)
	for off+60 <= len(b) {
		hdr := b[off : off+60]
		if string(hdr[58:60]) != "`\n" {
			return nil, errors.New("bad ar member header")
		}
		n := strings.TrimRight(strings.TrimSpace(string(hdr[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 || off+60+int(size) > len(b) {
			return nil, errors.New("bad ar member size")
		}
		body := b[off+60 : off+60+int(size)]
		if n == name {
			return body, nil
		}
		off += 60 + int(size)
		if size%2 == 1 {
			off++ // members are 2-byte aligned
		}
	}
	return nil, fmt.Errorf("no %s member", name)
}
