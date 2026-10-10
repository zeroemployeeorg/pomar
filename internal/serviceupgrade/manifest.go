package serviceupgrade

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

type FilePin struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type Build struct {
	Pin      string             `json:"source_commit"`
	HostPin  string             `json:"host_source_commit"`
	Binaries map[string]FilePin `json:"binaries"`
}

// The manifest names fixed product roles, never arbitrary target paths or
// shell commands. A separate administrator-retained SHA256 anchors its bytes.
type Manifest struct {
	Schema          string             `json:"schema"`
	Host            string             `json:"host"`
	OperatorUID     uint32             `json:"operator_uid"`
	OwnerUID        uint32             `json:"owner_uid"`
	CallerUID       uint32             `json:"caller_uid"`
	Baseline        Build              `json:"baseline"`
	Target          Build              `json:"target"`
	InstallRecord   FilePin            `json:"install_record"`
	Retained        map[string]FilePin `json:"retained"`
	KeyID           string             `json:"manager_key_id"`
	CertificateSHA1 string             `json:"certificate_sha1"`
	SigningPolicy   string             `json:"signing_policy"`
	Entitlements    FilePin            `json:"host_entitlements"`
}

var hex40 = regexp.MustCompile(`^[a-f0-9]{40}$`)
var hex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)

var binaries = []string{"pomar", "pomar-host"}
var retained = []string{"shim", "caller", "caller-policy", "caller-grant", "classes", "plist"}

func validFile(f FilePin, maximum int64) bool {
	return hex64.MatchString(f.SHA256) && f.Bytes > 0 && f.Bytes <= maximum
}

func (m Manifest) Validate() error {
	if m.Schema != "pomar.service-upgrade/v1" || !hostName.MatchString(m.Host) ||
		m.OperatorUID == 0 || m.OwnerUID == 0 || m.CallerUID == 0 || m.OperatorUID == m.OwnerUID ||
		m.OperatorUID == m.CallerUID || m.OwnerUID == m.CallerUID || !hex64.MatchString(m.KeyID) ||
		!validFile(m.InstallRecord, 64<<10) || !validFile(m.Entitlements, 64<<10) {
		return fmt.Errorf("unsupported service identity or incomplete certificate binding")
	}
	switch m.SigningPolicy {
	case "internal_certificate":
		if !hex40.MatchString(m.CertificateSHA1) {
			return fmt.Errorf("independently bound internal certificate required")
		}
	case "development_transition":
		if m.CertificateSHA1 != "" {
			return fmt.Errorf("development transition must not claim a release certificate")
		}
	default:
		return fmt.Errorf("explicit reviewed signing policy required")
	}
	for _, b := range []Build{m.Baseline, m.Target} {
		if !hex40.MatchString(b.Pin) || !hex40.MatchString(b.HostPin) || len(b.Binaries) != len(binaries) {
			return fmt.Errorf("two exact source-pinned binaries required")
		}
		for _, name := range binaries {
			if !validFile(b.Binaries[name], 512<<20) {
				return fmt.Errorf("missing or oversized binary binding")
			}
		}
	}
	if m.Baseline.Pin == m.Target.Pin || len(m.Retained) != len(retained) {
		return fmt.Errorf("a distinct target and complete retained state binding required")
	}
	for _, name := range retained {
		if !validFile(m.Retained[name], 512<<20) {
			return fmt.Errorf("missing retained file binding")
		}
	}
	return nil
}

func Decode(raw []byte, expectedSHA string) (Manifest, error) {
	var m Manifest
	if len(raw) > 64<<10 || !hex64.MatchString(expectedSHA) {
		return m, fmt.Errorf("bounded manifest and independently retained SHA256 required")
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != expectedSHA {
		return m, fmt.Errorf("manifest does not match its independently retained hash")
	}
	if err := uniqueJSON(raw); err != nil {
		return m, fmt.Errorf("duplicate, malformed or over-nested manifest fields")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid upgrade manifest")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, fmt.Errorf("trailing upgrade manifest data")
	}
	return m, m.Validate()
}

// Reject duplicate keys before decoding into maps or structs: a frozen digest
// must not give different meanings to different release tooling.
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("nesting")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := k.(string)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate key")
					}
					seen[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return fmt.Errorf("object")
				}
			case '[':
				for d.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return fmt.Errorf("array")
				}
			default:
				return fmt.Errorf("delimiter")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
