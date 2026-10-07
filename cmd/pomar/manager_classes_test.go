package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func profileFixture() []classProfile {
	return []classProfile{
		{Name: "ci", Image: "golang-ci", VCPU: 4, MemoryMiB: 6144, DiskPeakGiB: 3, Concurrency: 3, TimeLimitS: 1800, Callers: []string{"build"}, SourceMirrors: []string{"source"}, GoProxy: true},
		{Name: "site", Image: "node24-full", VCPU: 2, MemoryMiB: 8192, DiskPeakGiB: 8, Concurrency: 1, TimeLimitS: 10800, Callers: []string{"site-build"}, SourceMirrors: []string{"site"}, SourceRef: "main", Command: []string{"make", "verify"}, JobUser: true, NPM: true},
	}
}

func writeProfiles(t *testing.T, profiles []classProfile) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"schema": classesSchema, "classes": profiles})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "classes.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureUID(name string) (uint32, error) {
	if name == "build" {
		return 501, nil
	}
	if name == "site-build" {
		return 502, nil
	}
	return 0, fmt.Errorf("no local account %q", name)
}

func TestProfilesKeepPerClassImagesSourcesCallersAndLimits(t *testing.T) {
	profiles, allow, err := readClassProfiles(writeProfiles(t, profileFixture()), fixtureUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allow) != 2 || len(allow["ci"]) != 1 || allow["ci"][0] != 501 || allow["site"][0] != 502 {
		t.Fatalf("allow: %v", allow)
	}
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	classes, pinned := configuredClasses(profiles, &smoke.Env{Venue: v}, "helper", "kernel")
	if len(classes) != 2 || classes[0].Guest.ImageDigest == classes[1].Guest.ImageDigest {
		t.Fatal("images collapsed")
	}
	if classes[1].Class.TimeLimitSeconds != 10800 || classes[1].Class.MemoryBytes != 8192<<20 || classes[1].Class.Concurrency != 1 || classes[1].Class.Measured {
		t.Fatalf("site limits: %+v", classes[1].Class)
	}
	if classes[0].DisableGoProxy || !classes[1].DisableGoProxy || !classes[1].NPM || !classes[1].JobUser || classes[1].SourceMirrors[0] != "site" {
		t.Fatal("class policy lost")
	}
	paths := pinned()
	if len(paths) != 3 || paths[1] == paths[2] {
		t.Fatalf("pinned paths: %v", paths)
	}
	paths[0] = "changed"
	if pinned()[0] == "changed" {
		t.Fatal("pinned collector exposed mutable slice")
	}
	for _, c := range classes {
		if _, err := c.Base(); err != nil {
			t.Fatal(err)
		}
		if err := c.VerifyBase(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProfilesRefuseUnresolvedOrAmbiguousAuthority(t *testing.T) {
	for name, change := range map[string]func([]classProfile){
		"unknown account":      func(p []classProfile) { p[1].Callers = []string{"missing"} },
		"no callers":           func(p []classProfile) { p[1].Callers = nil },
		"no source list":       func(p []classProfile) { p[1].SourceMirrors = nil },
		"duplicate caller":     func(p []classProfile) { p[1].Callers = []string{"build", "build"} },
		"duplicate class":      func(p []classProfile) { p[1].Name = "ci" },
		"unreviewed image":     func(p []classProfile) { p[1].Image = "custom-url" },
		"no time bound":        func(p []classProfile) { p[1].TimeLimitS = 0 },
		"overflow memory":      func(p []classProfile) { p[1].MemoryMiB = 1 << 50 },
		"negative disk":        func(p []classProfile) { p[1].DiskPeakGiB = -1 },
		"no concurrency bound": func(p []classProfile) { p[1].Concurrency = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			p := profileFixture()
			change(p)
			if _, _, err := readClassProfiles(writeProfiles(t, p), fixtureUID); err == nil {
				t.Fatal("bad profile accepted")
			}
		})
	}
	p := writeProfiles(t, profileFixture())
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"duplicate field":   bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"other","schema":`), 1),
		"unknown field":     bytes.Replace(raw, []byte(`"schema":`), []byte(`"extra":1,"schema":`), 1),
		"trailing document": append(append([]byte(nil), raw...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "bad.json")
			os.WriteFile(p, b, 0600)
			if _, _, err := readClassProfiles(p, fixtureUID); err == nil {
				t.Fatal("ambiguous document accepted")
			}
		})
	}
	if err := os.Chmod(p, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readClassProfiles(p, fixtureUID); err == nil {
		t.Fatal("writable profile accepted")
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readClassProfiles(link, fixtureUID); err == nil {
		t.Fatal("symlink profile accepted")
	}
}

func TestClassFileRejectsLegacyOverridesBeforeVenueAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	var out, errb bytes.Buffer
	got := managerCmd([]string{"-root", root, "-classes-file", "missing.json", "-class-name", "legacy"}, &out, &errb)
	if got != 2 || !strings.Contains(errb.String(), "cannot be combined") {
		t.Fatalf("%d: %s", got, errb.String())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("invalid configuration opened the venue")
	}
}
