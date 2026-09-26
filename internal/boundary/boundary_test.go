package boundary

import (
	"strings"
	"testing"
)

// good is what the probe reports in a guest that passes: measured in POMAR-
// SOW-06 §3, with a read-only source and no resolver.
const good = `uid=1000
iface=lo
tcp=refused
dns=failed
nameservers=0
work=readonly
env=GOPATH
env=HOME
env=PATH
file=/pomar/pins.json
file=/pomar/inputs/probe.sh
file=/pomar/shim
end=1
`

func TestGradePasses(t *testing.T) {
	r := Grade(good)
	if !r.Pass || len(r.Checks) != 4 {
		t.Fatalf("report = %+v", r)
	}
	for i, name := range []string{CheckNetwork, CheckHome, CheckReadonly, CheckNoToken} {
		if r.Checks[i].Name != name || !r.Checks[i].Pass {
			t.Errorf("check %d = %+v", i, r.Checks[i])
		}
	}
}

func TestGradeFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		edit  func(string) string
		fails string
	}{
		"an interface":       {func(s string) string { return strings.Replace(s, "iface=lo\n", "iface=lo\niface=eth0\n", 1) }, CheckNetwork},
		"a public connect":   {func(s string) string { return strings.Replace(s, "tcp=refused", "tcp=open", 1) }, CheckNetwork},
		"tcp untestable":     {func(s string) string { return strings.Replace(s, "tcp=refused", "tcp=untestable", 1) }, CheckNetwork},
		"a resolved name":    {func(s string) string { return strings.Replace(s, "dns=failed", "dns=resolved", 1) }, CheckNetwork},
		"a resolver named":   {func(s string) string { return strings.Replace(s, "nameservers=0", "nameservers=2", 1) }, CheckNetwork},
		"a host path":        {func(s string) string { return strings.Replace(s, "end=1", "hostpath=/Users\nend=1", 1) }, CheckHome},
		"a writable source":  {func(s string) string { return strings.Replace(s, "work=readonly", "work=writable", 1) }, CheckReadonly},
		"the job as root":    {func(s string) string { return strings.Replace(s, "uid=1000", "uid=0", 1) }, CheckReadonly},
		"a token in env":     {func(s string) string { return strings.Replace(s, "env=PATH", "env=PATH\nenv=GITHUB_TOKEN", 1) }, CheckNoToken},
		"an ssh agent":       {func(s string) string { return strings.Replace(s, "env=PATH", "env=PATH\nenv=SSH_AUTH_SOCK", 1) }, CheckNoToken},
		"an unexpected file": {func(s string) string { return strings.Replace(s, "end=1", "file=/pomar/keys/id\nend=1", 1) }, CheckNoToken},
		"a lookalike file":   {func(s string) string { return strings.Replace(s, "end=1", "file=/pomar/pins.json.bak\nend=1", 1) }, CheckNoToken},
		"a repeated key":     {func(s string) string { return strings.Replace(s, "work=readonly", "work=readonly\nwork=readonly", 1) }, CheckReadonly},
	} {
		r := Grade(tc.edit(good))
		if r.Pass {
			t.Errorf("%s: passed", name)
		}
		for _, c := range r.Checks {
			if c.Name == tc.fails && c.Pass {
				t.Errorf("%s: %s passed", name, c.Name)
			}
		}
	}
}

// A report cut short, or empty, fails every check.
func TestGradeIncomplete(t *testing.T) {
	for _, s := range []string{"", strings.Replace(good, "end=1\n", "", 1)} {
		r := Grade(s)
		for _, c := range r.Checks {
			if c.Pass {
				t.Errorf("%s passed on an incomplete report", c.Name)
			}
		}
	}
}
