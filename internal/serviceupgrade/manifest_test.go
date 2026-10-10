package serviceupgrade

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixtureManifest() Manifest {
	f := FilePin{strings.Repeat("a", 64), 123}
	b := Build{Pin: strings.Repeat("b", 40), HostPin: strings.Repeat("c", 40), Binaries: map[string]FilePin{"pomar": f, "pomar-host": f}}
	m := Manifest{Schema: "pomar.service-upgrade/v1", Host: "mac-example", OperatorUID: 501, OwnerUID: 410, CallerUID: 65020, Baseline: b, Target: b, InstallRecord: f, KeyID: strings.Repeat("d", 64), CertificateSHA1: strings.Repeat("e", 40), Entitlements: f, Retained: map[string]FilePin{}}
	m.Target.Pin = strings.Repeat("f", 40)
	m.SigningPolicy = "internal_certificate"
	for _, n := range retained {
		m.Retained[n] = f
	}
	return m
}
func TestManifestRejectsAmbiguousOrUnboundPayloads(t *testing.T) {
	m := fixtureManifest()
	raw, _ := json.Marshal(m)
	if _, e := Decode(raw, digest(raw)); e != nil {
		t.Fatal(e)
	}
	for name, edit := range map[string]func([]byte) []byte{
		"wrong anchor": func(b []byte) []byte { return append(b, ' ') },
		"duplicate field": func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"host":`, `"host":"other","host":`, 1))
		},
		"unknown field":   func(b []byte) []byte { return append([]byte(`{"command":"/bin/sh",`), b[1:]...) },
		"trailing object": func(b []byte) []byte { return append(b, []byte(`{}`)...) },
	} {
		t.Run(name, func(t *testing.T) {
			b := edit(raw)
			sha := digest(b)
			if name == "wrong anchor" {
				sha = digest(raw)
			}
			if _, e := Decode(b, sha); e == nil {
				t.Fatal("accepted ambiguous manifest")
			}
		})
	}
	m.Target.Binaries["/etc/sudoers"] = m.InstallRecord
	raw, _ = json.Marshal(m)
	if _, e := Decode(raw, digest(raw)); e == nil {
		t.Fatal("accepted arbitrary path")
	}
}
