//go:build darwin

package serviceupgrade

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"github.com/zeroemployeeorg/pomar/internal/manager"
	"strings"
	"testing"
)

func TestHistoryRetainsUnknownEvidenceAndRefusesLiveOrDuplicateEntries(t *testing.T) {
	valid := []byte(`[{"attempt":"old-1","state":"exited","command":["true"],"new_future_evidence":{"a":1},"class":{"name":"allowed"}}]`)
	h, e := history(valid, "allowed")
	if e != nil {
		t.Fatal(e)
	}
	changed := bytes.Replace(valid, []byte(`"a":1`), []byte(`"a":2`), 1)
	other, e := history(changed, "allowed")
	if e != nil || h == other {
		t.Fatal("future fields lost", e)
	}
	for _, bad := range [][]byte{bytes.Replace(valid, []byte(`"exited"`), []byte(`"running"`), 1), []byte(`[{"attempt":"x","state":"exited"},{"attempt":"x","state":"exited"}]`), []byte(`[{"attempt":"x","state":"exited","state":"running"}]`), []byte(`{}`), []byte(`null`)} {
		if _, e := history(bad, ""); e == nil {
			t.Fatal("unsafe history accepted", string(bad))
		}
	}
	if _, e := history(valid, "other"); e == nil {
		t.Fatal("caller class leak accepted")
	}
}
func TestKeyRequiresTheIndependentlyBoundPublicBytes(t *testing.T) {
	pub := bytes.Repeat([]byte{7}, 32)
	k := manager.KeyReply{Algorithm: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub), KeyID: digest(pub)}
	raw, _ := json.Marshal(k)
	if e := key(raw, k.KeyID); e != nil {
		t.Fatal(e)
	}
	k.PublicKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	raw, _ = json.Marshal(k)
	if e := key(raw, k.KeyID); e == nil {
		t.Fatal("self-asserted key ID trusted")
	}
}
func TestRecordTransitionClearsQualificationsAndRetainsUnknownBindings(t *testing.T) {
	m := fixtureManifest()
	raw := []byte("pin=" + m.Baseline.Pin + "\nkey_id=" + m.KeyID + "\nsha256_pomar=old\nsha256_pomar-host=old\nsha256_pomar-shim-linux-arm64=keep-shim\ncheck_ok=yes\nreboot_ok=yes\nconcurrency=9\nvalidation=old qualification\nfuture_binding=keep\n")
	b, e := targetRecord(raw, m, "/private/var/pomar-p-archive/product-example")
	if e != nil {
		t.Fatal(e)
	}
	r, e := record(b)
	if e != nil {
		t.Fatal(e)
	}
	if r["check_ok"] != "" || r["reboot_ok"] != "" || r["concurrency"] != "" || r["future_binding"] != "keep" || r["key_id"] != m.KeyID || r["sha256_pomar-shim-linux-arm64"] != "keep-shim" || r["pin"] != m.Target.Pin || r["retained_host_pin"] != m.Target.HostPin {
		t.Fatal(r)
	}
	pending, e := invalidatedRecord(raw)
	if e != nil {
		t.Fatal(e)
	}
	pr, e := record(pending)
	if e != nil || pr["pin"] != m.Baseline.Pin || pr["check_ok"] != "" || pr["validation"] == "old qualification" {
		t.Fatal(pr, e)
	}
	for _, raw := range []string{"x=\nx=other\n", "x=one\nx=two\n", "not-a-record\n"} {
		if _, e := record([]byte(raw)); e == nil {
			t.Fatal("duplicate/invalid record accepted")
		}
	}
	if !strings.Contains(string(b), "prior qualification invalidated") {
		t.Fatal("qualification falsely restored")
	}
}
func TestCapacityBindingIgnoresMeasurementsButRetainsPolicy(t *testing.T) {
	a := []byte(`{"live":0,"draining":false,"class":{"name":"test","vcpu":2,"memory_bytes":8589934592},"classes":[{"name":"test","vcpu":2,"memory_bytes":8589934592}],"host":{"cpu_slots":2,"memory_bytes":8589934592},"space":{"avail":5}}`)
	b := bytes.ReplaceAll(a, []byte(`"draining":false`), []byte(`"draining":true`))
	b = bytes.ReplaceAll(b, []byte(`"avail":5`), []byte(`"avail":1`))
	h, e := capacity(a)
	if e != nil {
		t.Fatal(e)
	}
	k, e := capacity(b)
	if e != nil || h != k {
		t.Fatal("mutable observations changed identity", e)
	}
	b = bytes.ReplaceAll(b, []byte(`"vcpu":2`), []byte(`"vcpu":4`))
	k, e = capacity(b)
	if e != nil || h == k {
		t.Fatal("policy change hidden", e)
	}
	if _, e := capacity([]byte(`{"live":1}`)); e == nil {
		t.Fatal("live capacity accepted")
	}
}
func TestDevelopmentSignatureAcceptsActualLinkerAndHelperFlagForms(t *testing.T) {
	for _, flags := range []string{"0x2(adhoc)", "0x20002(adhoc,linker-signed)"} {
		if !isDevelopmentSignature("Identifier=example\nCodeDirectory v=20400 flags=" + flags + " hashes=1+0\nSignature=adhoc\n") {
			t.Fatal(flags)
		}
	}
	for _, bad := range []string{"Signature=adhoc\n", "Identifier=example\nCodeDirectory flags=0x0(adhoc)\nSignature=adhoc\n", "Identifier=example\nCodeDirectory flags=0x2(adhoc)\nSignature=release\n"} {
		if isDevelopmentSignature(bad) {
			t.Fatal("unbound signature accepted")
		}
	}
}
func TestSigningPolicyNeverSilentlyDowngrades(t *testing.T) {
	m := fixtureManifest()
	m.SigningPolicy = ""
	if e := m.Validate(); e == nil {
		t.Fatal("missing policy accepted")
	}
	m.SigningPolicy = "development_transition"
	if e := m.Validate(); e == nil {
		t.Fatal("false certificate claim accepted")
	}
	m.CertificateSHA1 = ""
	if e := m.Validate(); e != nil {
		t.Fatal(e)
	}
	m.SigningPolicy = "internal_certificate"
	if e := m.Validate(); e == nil {
		t.Fatal("internal signing without certificate accepted")
	}
}
func TestMissingLaunchdLabelRequiresACompleteSuccessfulInventory(t *testing.T) {
	if e := serviceAbsentFromTable([]byte("PID\tStatus\tLabel\n123\t0\tother.service\n")); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"", "Could not find service", "PID Status Label\n- 0 " + label + "\n", "PID Status Label\nmalformed\n"} {
		if e := serviceAbsentFromTable([]byte(bad)); e == nil {
			t.Fatal("absence inferred from uncertain evidence")
		}
	}
}
