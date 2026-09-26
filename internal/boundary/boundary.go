// Package boundary is the guest boundary self-test (POMAR-SOW-06 §4.3; the
// elders' ruling r26 §4.3): a fixed probe that runs as the job in a real
// guest and reports what it can reach, and the host-side checks that grade
// its report. The probe only reports; every judgement is made here, in Go,
// on the host. A missing or untestable line fails its check.
package boundary

import (
	"bufio"
	"regexp"
	"strings"
)

// Probe is the script the job runs, as an input, with a read-only source.
// It writes key=value lines to its output, and ends with end=1 so that a
// cut-short report fails.
const Probe = `#!/bin/sh
out=/pomar/outputs/` + ReportName + `
{
echo "uid=$(id -u)"
for i in /sys/class/net/*; do echo "iface=${i##*/}"; done
if command -v bash >/dev/null 2>&1; then
	if timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null; then echo tcp=open; else echo tcp=refused; fi
else
	echo tcp=untestable
fi
if command -v getent >/dev/null 2>&1; then
	if getent hosts example.com >/dev/null 2>&1; then echo dns=resolved; else echo dns=failed; fi
else
	echo dns=untestable
fi
n=$(grep -c '^nameserver' /etc/resolv.conf 2>/dev/null); echo "nameservers=${n:-0}"
for p in /Users /Volumes /private; do [ -e "$p" ] && echo "hostpath=$p"; done
if touch /work/.pomar-boundary 2>/dev/null; then echo work=writable; rm -f /work/.pomar-boundary; else echo work=readonly; fi
env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/env=\1/p'
find /pomar -type f 2>/dev/null | while IFS= read -r f; do [ -r "$f" ] && echo "file=$f"; done
echo end=1
} > "$out" 2>/dev/null
cat "$out"
`

// ReportName is the probe's output, copied out of the guest.
const ReportName = "boundaries.txt"

// The four checks, with the names profrod-site's workshop uses (#744 item 7).
const (
	CheckNetwork  = "candidate_cannot_reach_public_network"
	CheckHome     = "candidate_cannot_read_home"
	CheckReadonly = "candidate_code_is_readonly"
	CheckNoToken  = "candidate_has_no_admin_token"
)

// Check is one graded check.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// Report is the graded report.
type Report struct {
	Pass   bool    `json:"pass"`
	Checks []Check `json:"checks"`
}

// secretName is an environment name that looks like it carries a credential.
var secretName = regexp.MustCompile(`(?i)token|secret|passw|credential|auth|api_?key|private_?key|access_?key`)

// readableOK are the files under /pomar a job may read: its inputs, its
// outputs, its own home, its pins, and Pomar's module proxy shim.
var readableOK = []string{"/pomar/inputs/", "/pomar/outputs/", "/pomar/job/", "/pomar/pins.json", "/pomar/shim", "/pomar/shim.ready"}

// Grade checks a probe report.
func Grade(report string) Report {
	kv := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(report))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[k] = append(kv[k], v)
		}
	}
	one := func(k string) string {
		if v := kv[k]; len(v) == 1 {
			return v[0]
		}
		return ""
	}
	complete := one("end") == "1"
	var r Report

	// The network: loopback only, a public connect refused, a public name
	// unresolved, and no resolver named.
	net := Check{Name: CheckNetwork}
	switch {
	case !complete:
		net.Detail = "report incomplete"
	case len(kv["iface"]) != 1 || kv["iface"][0] != "lo":
		net.Detail = "interfaces: " + strings.Join(kv["iface"], ",")
	case one("tcp") != "refused":
		net.Detail = "public tcp connect: " + one("tcp")
	case one("dns") != "failed":
		net.Detail = "public name lookup: " + one("dns")
	case one("nameservers") != "0":
		net.Detail = "resolv.conf names " + one("nameservers") + " resolver(s)"
	default:
		net.Pass, net.Detail = true, "lo only; tcp refused; dns failed; no resolver"
	}

	home := Check{Name: CheckHome}
	switch {
	case !complete:
		home.Detail = "report incomplete"
	case len(kv["hostpath"]) > 0:
		home.Detail = "host paths present: " + strings.Join(kv["hostpath"], ",")
	default:
		home.Pass, home.Detail = true, "no /Users, /Volumes or /private"
	}

	ro := Check{Name: CheckReadonly}
	switch {
	case !complete:
		ro.Detail = "report incomplete"
	case one("work") != "readonly":
		ro.Detail = "/work: " + one("work")
	case one("uid") == "0" || one("uid") == "":
		ro.Detail = "the job ran as uid " + one("uid")
	default:
		ro.Pass, ro.Detail = true, "/work refused a write from uid "+one("uid")
	}

	tok := Check{Name: CheckNoToken}
	var bad []string
	for _, n := range kv["env"] {
		if secretName.MatchString(n) {
			bad = append(bad, "env "+n)
		}
	}
	for _, f := range kv["file"] {
		if !allowed(f) {
			bad = append(bad, "file "+f)
		}
	}
	switch {
	case !complete:
		tok.Detail = "report incomplete"
	case len(kv["env"]) == 0:
		tok.Detail = "no environment reported"
	case len(bad) > 0:
		tok.Detail = strings.Join(bad, "; ")
	default:
		tok.Pass, tok.Detail = true, "no credential-like environment; no readable file under /pomar beyond inputs, outputs, home, pins and shim"
	}

	r.Checks = []Check{net, home, ro, tok}
	r.Pass = net.Pass && home.Pass && ro.Pass && tok.Pass
	return r
}

func allowed(f string) bool {
	for _, a := range readableOK {
		if strings.HasSuffix(a, "/") && strings.HasPrefix(f, a) || f == a {
			return true
		}
	}
	return false
}
