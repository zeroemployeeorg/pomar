package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/base"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/manager"
	"github.com/zeroemployeeorg/pomar/internal/smoke"
)

const classesSchema = "pomar.manager-classes/v1"

type classProfile struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	VCPU           int      `json:"vcpu"`
	MemoryMiB      int64    `json:"memory_mib"`
	DiskPeakGiB    int64    `json:"disk_peak_gib"`
	Concurrency    int      `json:"concurrency"`
	TimeLimitS     int64    `json:"time_limit_s"`
	LogCapBytes    int64    `json:"log_cap_bytes"`
	OutputCapBytes int64    `json:"output_cap_bytes"`
	Callers        []string `json:"callers"`
	SourceMirrors  []string `json:"source_mirrors"`
	SourceRef      string   `json:"source_ref,omitempty"`
	Command        []string `json:"command,omitempty"`
	JobUser        bool     `json:"job_user"`
	GoProxy        bool     `json:"go_proxy"`
	NPM            bool     `json:"npm"`
	NPMLock        string   `json:"npm_lock,omitempty"`
}

// Read this owner-supplied configuration before opening a venue or creating a
// signing key. A profile selects reviewed catalogue images, never image URLs.
func readClassProfiles(file string, lookup func(string) (uint32, error)) ([]classProfile, map[string][]uint32, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() > 1<<20 {
		return nil, nil, fmt.Errorf("classes file must be a regular file, at most 1 MiB, with no group/other write permission")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, nil, fmt.Errorf("classes file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, nil, fmt.Errorf("cannot read bounded classes file")
	}
	if err := uniqueClassJSON(json.NewDecoder(bytes.NewReader(b)), 0); err != nil {
		return nil, nil, err
	}
	var doc struct {
		Schema  string         `json:"schema"`
		Classes []classProfile `json:"classes"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&doc); err != nil {
		return nil, nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, nil, fmt.Errorf("classes file must contain one JSON document")
	}
	if doc.Schema != classesSchema || len(doc.Classes) == 0 || len(doc.Classes) > 32 {
		return nil, nil, fmt.Errorf("classes file requires %s and 1–32 classes", classesSchema)
	}
	namePattern := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	allow := map[string][]uint32{}
	for i := range doc.Classes {
		c := &doc.Classes[i]
		if !namePattern.MatchString(c.Name) || allow[c.Name] != nil {
			return nil, nil, fmt.Errorf("classes file has an invalid or repeated class name")
		}
		if _, err := smoke.GuestImageByName(c.Image); err != nil {
			return nil, nil, err
		}
		if c.VCPU <= 0 || c.MemoryMiB <= 0 || c.MemoryMiB > (math.MaxInt64-capacity.VMOverhead)>>20 ||
			c.DiskPeakGiB < 0 || c.DiskPeakGiB > (math.MaxInt64-capacity.HeadroomBytes)>>30 || c.Concurrency <= 0 ||
			c.TimeLimitS <= 0 || c.TimeLimitS > math.MaxInt64/int64(time.Second) || c.LogCapBytes < 0 || c.LogCapBytes > 64<<20 {
			return nil, nil, fmt.Errorf("class %q has invalid resource limits", c.Name)
		}
		// Profile-driven classes share a 64 MiB transport budget between
		// the output log and named files. Materialise defaults before admission
		// so the result records the limits actually enforced.
		if c.LogCapBytes == 0 {
			c.LogCapBytes = manager.DefaultLogCapBytes
		}
		if c.OutputCapBytes == 0 {
			c.OutputCapBytes = manager.MaxOutputsBytes - c.LogCapBytes
		}
		if c.OutputCapBytes <= 0 || c.OutputCapBytes > manager.MaxOutputsBytes-c.LogCapBytes {
			return nil, nil, fmt.Errorf("class %q output and log caps must fit within 64 MiB", c.Name)
		}
		if len(c.Callers) == 0 || len(c.SourceMirrors) == 0 {
			return nil, nil, fmt.Errorf("class %q requires explicit callers and source mirrors", c.Name)
		}
		seen := map[uint32]bool{}
		for _, user := range c.Callers {
			uid, err := lookup(user)
			if err != nil {
				return nil, nil, err
			}
			if seen[uid] {
				return nil, nil, fmt.Errorf("class %q repeats a caller uid", c.Name)
			}
			allow[c.Name] = append(allow[c.Name], uid)
			seen[uid] = true
		}
	}
	return doc.Classes, allow, nil
}

func configuredClasses(profiles []classProfile, e *smoke.Env, hostBin, kernelSHA string) ([]manager.JobClass, func() []string) {
	// All base closures capture each reviewed image, not the last iteration.
	classes := make([]manager.JobClass, 0, len(profiles))
	pinned := []string{e.KernelPath()}
	for _, p := range profiles {
		gi, _ := smoke.GuestImageByName(p.Image) // validated before venue access
		bs := &base.Bases{Venue: e.Venue, HostBin: hostBin, PackageSet: gi.PackageSet()}
		basePath, _ := bs.Path(gi.Arm64)
		pinned = append(pinned, basePath)
		c := ciClass(p.Name, p.VCPU, p.MemoryMiB, p.DiskPeakGiB, p.Concurrency, time.Duration(p.TimeLimitS)*time.Second)
		c.LogCapBytes = p.LogCapBytes
		c.OutputCapBytes = p.OutputCapBytes
		classes = append(classes, manager.JobClass{
			Class: c, Guest: manager.Guest{Kernel: e.KernelPath(), KernelSHA256: kernelSHA,
				InitRef: smoke.InitRepo + "@" + smoke.InitDigest, InitDigest: smoke.InitDigest,
				ImageRef: gi.Ref(), ImageDigest: gi.Digest, ImageArm64: gi.Arm64, PackageSet: gi.PackageSet()},
			JobUser: p.JobUser, NPM: p.NPM, NPMLock: p.NPMLock,
			SourceMirrors: p.SourceMirrors, SourceRef: p.SourceRef, Command: p.Command, DisableGoProxy: !p.GoProxy,
			Base: func() (string, error) {
				if !bs.Exists(gi.Arm64) {
					return "", nil
				}
				return bs.Verified(gi.Arm64)
			},
			VerifyBase: func() error {
				if !bs.Exists(gi.Arm64) {
					return nil
				}
				_, err := bs.Verify(gi.Arm64)
				return err
			},
		})
	}
	return classes, func() []string { return append([]string(nil), pinned...) }
}

func uniqueClassJSON(d *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("classes JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("classes JSON has a duplicate field")
			}
			seen[name] = true
			if err := uniqueClassJSON(d, depth+1); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for d.More() {
			if err := uniqueClassJSON(d, depth+1); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("classes JSON has an invalid container")
	}
	_, err = d.Token()
	return err
}
