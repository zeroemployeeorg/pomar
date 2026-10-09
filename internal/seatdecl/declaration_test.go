package seatdecl

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

const pin = "c3e64a5000000000000000000000000000000001"
const tree = "7ee0000000000000000000000000000000000002"

// valid is the pilot's shape: zeocreator, its role and provider account not
// yet allocated, so no App is named (SOW 16 §1.5, §8).
func valid() Declaration {
	return Declaration{
		Schema: Schema, Seat: "zeocreator", Project: "zeocreator",
		Sources: []Source{
			{Role: "work", Repository: "zeroemployeeorg/zeocreator", Fork: "architect/zeocreator", Branch: "principal/urls", Commit: pin, Tree: tree, Destination: "work", Writable: true,
				Locks: []Lock{{Path: "uv.lock", SHA256: strings.Repeat("a", 64)}}},
			{Role: "records", Repository: "owner/records", Fork: "architect/records", Branch: "records/zeocreator", Commit: pin, Tree: tree, Destination: "records", Writable: true},
			{Role: "read_only", Repository: "owner/zeocore", Branch: "main", Commit: pin, Tree: tree, Destination: "deps/zeocore"},
		},
		Class:     "seat-linux-arm64-v1",
		Resources: Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
		Network:   Network{AllowedHosts: []string{"api.anthropic.com"}},
		Provider:  Provider{Agent: "claude", Version: "2.1.280", Account: NotEstablished},
		Resume:    Resume{Policy: "fresh-from-sow", HandoverSOW: "records/zeocreator@abc:SOW-04"},
		Identity: Identity{Role: NotEstablished, Forks: []string{"architect/zeocreator", "architect/records"},
			Push:        []string{"refs/heads/zeocreator/*", "refs/heads/records/zeocreator"},
			Permissions: map[string]string{"contents": "write", "metadata": "read"}},
	}
}

func encode(t *testing.T, d Declaration) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheValidDeclarationParsesAndItsHashIsItsIdentity(t *testing.T) {
	raw := encode(t, valid())
	d, sum, err := Parse(raw)
	if err != nil || d.Seat != "zeocreator" || len(sum) != 64 {
		t.Fatalf("%v %q", err, sum)
	}
	if _, again, _ := Parse(raw); again != sum {
		t.Fatal("the same bytes gave another hash")
	}
	b := valid()
	b.Class = "seat-linux-arm64-v2"
	if _, other, _ := Parse(encode(t, b)); other == sum {
		t.Fatal("another declaration gave the same hash")
	}
}

func TestParseRefusesUnknownFieldsTrailingDataAndSize(t *testing.T) {
	raw := encode(t, valid())
	for name, b := range map[string][]byte{
		"an unknown field": []byte(strings.Replace(string(raw), `"seat":`, `"token":"x","seat":`, 1)),
		"trailing data":    append(append([]byte{}, raw...), []byte(` {}`)...),
		"empty":            nil,
		"over 64 KiB":      []byte(`{"schema":"` + strings.Repeat("x", Limit) + `"}`),
	} {
		if _, _, err := Parse(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every rule refuses its own violation; one mutation per case.
func TestValidateRefusesEachViolation(t *testing.T) {
	cases := map[string]func(*Declaration){
		"another schema":               func(d *Declaration) { d.Schema = "pomar.seat-manifest/v1" },
		"an upper-case seat":           func(d *Declaration) { d.Seat = "ZeoCreator" },
		"no work source":               func(d *Declaration) { d.Sources = d.Sources[1:] },
		"two records sources":          func(d *Declaration) { d.Sources = append(d.Sources, d.Sources[1]) },
		"a short commit":               func(d *Declaration) { d.Sources[0].Commit = "c3e64a5" },
		"a missing tree":               func(d *Declaration) { d.Sources[2].Tree = "" },
		"records without a pin":        func(d *Declaration) { d.Sources[1].Commit = "" },
		"work not writable":            func(d *Declaration) { d.Sources[0].Writable = false },
		"work on its golden repo":      func(d *Declaration) { d.Sources[0].Fork = "zeroemployeeorg/zeocreator" },
		"a writable read-only source":  func(d *Declaration) { d.Sources[2].Writable = true },
		"a read-only source with fork": func(d *Declaration) { d.Sources[2].Fork = "architect/zeocore" },
		"an absolute destination":      func(d *Declaration) { d.Sources[2].Destination = "/etc" },
		"an escaping destination":      func(d *Declaration) { d.Sources[2].Destination = "../up" },
		"an unclean destination":       func(d *Declaration) { d.Sources[2].Destination = "deps//zeocore" },
		"overlapping destinations":     func(d *Declaration) { d.Sources[2].Destination = "work/deps" },
		"equal destinations":           func(d *Declaration) { d.Sources[2].Destination = "records" },
		"a bad lock digest":            func(d *Declaration) { d.Sources[0].Locks[0].SHA256 = "abc" },
		"a branch with ..":             func(d *Declaration) { d.Sources[0].Branch = "a/../b" },
		"too many CPUs":                func(d *Declaration) { d.Resources.CPUs = 16 },
		"no memory":                    func(d *Declaration) { d.Resources.MemoryGiB = 0 },
		"a wildcard host":              func(d *Declaration) { d.Network.AllowedHosts = []string{"*.github.com"} },
		"an IP host":                   func(d *Declaration) { d.Network.AllowedHosts = []string{net.IPv4(192, 0, 2, 1).String()} },
		"an upper-case host":           func(d *Declaration) { d.Network.AllowedHosts = []string{"GitHub.com"} },
		"a duplicate host":             func(d *Declaration) { d.Network.AllowedHosts = []string{"github.com", "github.com"} },
		"an unpinned agent":            func(d *Declaration) { d.Provider.Version = "latest" },
		"an unknown agent":             func(d *Declaration) { d.Provider.Agent = "other" },
		"a credential as the account":  func(d *Declaration) { d.Provider.Account = "sk-ant-oat01-secret" },
		"no handover SOW":              func(d *Declaration) { d.Resume.HandoverSOW = "" },
		"an unknown resume policy":     func(d *Declaration) { d.Resume.Policy = "continue" },
		"an unknown role":              func(d *Declaration) { d.Identity.Role = "admin" },
		"an unbound role with an App":  func(d *Declaration) { d.Identity.AppID = 1 },
		"a bound role without an App": func(d *Declaration) {
			d.Identity.Role = "master"
			d.Identity.GitAuthor = "mator-master[bot] <x@users.noreply.github.com>"
		},
		"a bound role not as its bot": func(d *Declaration) {
			d.Identity.Role, d.Identity.AppID, d.Identity.InstallationID, d.Identity.GitAuthor = "master", 1, 2, "someone <a@b.c>"
		},
		"a golden repo as a fork":             func(d *Declaration) { d.Identity.Forks = []string{"zeroemployeeorg/zeocreator", "architect/records"} },
		"a fork that is also a golden source": func(d *Declaration) { d.Sources[2].Repository = "architect/records" },
		"forks not the writable set":          func(d *Declaration) { d.Identity.Forks = []string{"architect/zeocreator"} },
		"push to main":                        func(d *Declaration) { d.Identity.Push = []string{"refs/heads/main"} },
		"push to another seat":                func(d *Declaration) { d.Identity.Push = []string{"refs/heads/zeonewsroom/*"} },
		"push to another lane":                func(d *Declaration) { d.Identity.Push = []string{"refs/heads/records/zeonewsroom"} },
		"push to a tag":                       func(d *Declaration) { d.Identity.Push = []string{"refs/tags/zeocreator/v1"} },
		"push to a look-alike":                func(d *Declaration) { d.Identity.Push = []string{"refs/heads/zeocreatorx/a"} },
		"push to the bare seat ref":           func(d *Declaration) { d.Identity.Push = []string{"refs/heads/zeocreator"} },
		"push with a glob inside":             func(d *Declaration) { d.Identity.Push = []string{"refs/heads/zeocreator/*/x"} },
		"no push refs":                        func(d *Declaration) { d.Identity.Push = nil },
		"administration":                      func(d *Declaration) { d.Identity.Permissions["administration"] = "write" },
		"statuses":                            func(d *Declaration) { d.Identity.Permissions["statuses"] = "write" },
		"checks":                              func(d *Declaration) { d.Identity.Permissions["checks"] = "write" },
		"metadata write":                      func(d *Declaration) { d.Identity.Permissions["metadata"] = "write" },
		"workflows for non-steward":           func(d *Declaration) { d.Identity.Permissions["workflows"] = "write" },
		"no permissions":                      func(d *Declaration) { d.Identity.Permissions = nil },
		"a host-only case without a route": func(d *Declaration) {
			d.HostOnly = []HostOnly{{What: "keychain tests", Route: "elsewhere"}}
		},
	}
	for name, mutate := range cases {
		d := valid()
		d.Identity.Permissions = map[string]string{"contents": "write", "metadata": "read"}
		mutate(&d)
		if _, _, err := Parse(encode(t, d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateAcceptsTheAllowedShapes(t *testing.T) {
	cases := map[string]func(*Declaration){
		"a bound role as its bot": func(d *Declaration) {
			d.Identity.Role, d.Identity.AppID, d.Identity.InstallationID, d.Identity.GitAuthor = "master", 1, 2, "mator-master[bot] <1+mator-master[bot]@users.noreply.github.com>"
		},
		"a nested push ref":          func(d *Declaration) { d.Identity.Push = []string{"refs/heads/zeocreator/acceptance-20261009"} },
		"the records lane and below": func(d *Declaration) { d.Identity.Push = []string{"refs/heads/records/zeocreator/*"} },
		"contents read only":         func(d *Declaration) { d.Identity.Permissions["contents"] = "read" },
		"the steward with workflows": func(d *Declaration) {
			d.Identity.Role, d.Identity.AppID, d.Identity.InstallationID, d.Identity.GitAuthor = "steward", 1, 2, "mator-steward[bot] <x>"
			d.Identity.Permissions["workflows"] = "write"
		},
		"a host-only case":   func(d *Declaration) { d.HostOnly = []HostOnly{{What: "three Keychain tests", Route: "stays-on-host"}} },
		"no allowed hosts":   func(d *Declaration) { d.Network.AllowedHosts = nil },
		"a labelled account": func(d *Declaration) { d.Provider.Account = "claude-max-2" },
	}
	for name, mutate := range cases {
		d := valid()
		mutate(&d)
		if _, _, err := Parse(encode(t, d)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// The POMAR Codex's counterexamples (PR89 review at f8ffb220), as given.
func TestOwnerReviewStrictDeclarationRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*Declaration){
		"unallocated ledger role": func(d *Declaration) {
			d.Identity.Role = "ledger"
			d.Identity.AppID = 1
			d.Identity.InstallationID = 2
			d.Identity.GitAuthor = "unallocated[bot] <1+unallocated[bot]@users.noreply.github.com>"
		},
		"newline in allowed DNS name": func(d *Declaration) { d.Network.AllowedHosts = []string{"api.example.com\n"} },
		"tab in allowed DNS name":     func(d *Declaration) { d.Network.AllowedHosts = []string{"api.example.com\t"} },
		"parent directory lock path":  func(d *Declaration) { d.Sources[0].Locks[0].Path = ".." },
	} {
		t.Run(name, func(t *testing.T) {
			d := valid()
			mutate(&d)
			if _, _, err := Parse(encode(t, d)); err == nil {
				t.Fatal("invalid declaration was accepted")
			}
		})
	}
}

// The same boundary, more widely: the DNS grammar and inner paths.
func TestHostsAndPathsAreStrict(t *testing.T) {
	for name, mutate := range map[string]func(*Declaration){
		"a self lock path":       func(d *Declaration) { d.Sources[0].Locks[0].Path = "." },
		"a NUL in a lock path":   func(d *Declaration) { d.Sources[0].Locks[0].Path = "uv\x00.lock" },
		"a control in a dest":    func(d *Declaration) { d.Sources[2].Destination = "deps/zeo\x1bcore" },
		"an empty DNS label":     func(d *Declaration) { d.Network.AllowedHosts = []string{"api..example.com"} },
		"a trailing dot":         func(d *Declaration) { d.Network.AllowedHosts = []string{"api.example.com."} },
		"a leading hyphen label": func(d *Declaration) { d.Network.AllowedHosts = []string{"-api.example.com"} },
		"an underscore":          func(d *Declaration) { d.Network.AllowedHosts = []string{"api_x.example.com"} },
		"a single label":         func(d *Declaration) { d.Network.AllowedHosts = []string{"localhost"} },
		"a 64-character label":   func(d *Declaration) { d.Network.AllowedHosts = []string{strings.Repeat("a", 64) + ".com"} },
		"a non-ASCII host":       func(d *Declaration) { d.Network.AllowedHosts = []string{"api.exämple.com"} },
	} {
		d := valid()
		mutate(&d)
		if _, _, err := Parse(encode(t, d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d := valid()
	d.Network.AllowedHosts = []string{"api.anthropic.com", "files.pythonhosted.org", "a-b.c-d.example"}
	if _, _, err := Parse(encode(t, d)); err != nil {
		t.Fatalf("valid hosts refused: %v", err)
	}
}
