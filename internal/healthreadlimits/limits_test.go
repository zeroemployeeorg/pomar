package healthreadlimits

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestIndependentByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maximum int
		check   func([]byte) error
	}{
		{"line", MaxLineBytes, RequestLine}, {"block", MaxBlockBytes, HeaderBlock},
		{"header", MaxHeaderBytes, ProtectedHeader}, {"claims", MaxClaimsBytes, Claims},
		{"authorization", MaxAuthBytes, func(b []byte) error { return Authorization(string(b)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.check(bytes.Repeat([]byte{'A'}, tc.maximum)); err != nil {
				t.Fatal("equality rejected at byte gate", err)
			}
			if err := tc.check(bytes.Repeat([]byte{'A'}, tc.maximum+1)); !errors.Is(err, ErrSize) {
				t.Fatal("cap+1 accepted", err)
			}
		})
	}
}

func TestResourceSetLimitsAndExactBinding(t *testing.T) {
	ids := make([]string, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("%02d", i) + strings.Repeat("A", 126)
	}
	if err := Resources(ids); err != nil {
		t.Fatal(err)
	}
	reversed := append([]string{}, ids...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if err := SameResources(ids, reversed); err != nil {
		t.Fatal("order changed set", err)
	}
	if err := SameResources(ids, ids[:63]); !errors.Is(err, ErrSetMismatch) {
		t.Fatal("omission accepted", err)
	}
	for _, bad := range [][]string{nil, {}, {""}, {"x", "x"}, {"x/y"}, {"x%2Ey"}, {"x~y"}, {"é"}, {"x\r\ny"}, {strings.Repeat("A", 129)}, append(append([]string{}, ids...), "extra")} {
		if err := Resources(bad); err == nil {
			t.Fatal("invalid resource set accepted")
		}
	}
	if err := SameResources([]string{"a"}, []string{"b"}); !errors.Is(err, ErrSetMismatch) {
		t.Fatal("sibling accepted", err)
	}
}

func TestCardinalityDoesNotSelectFirstValue(t *testing.T) {
	for _, f := range []func([]string) error{SingleAuthorization, SingleIDs} {
		if err := f([]string{"one"}); err != nil {
			t.Fatal(err)
		}
		for _, values := range [][]string{nil, {"one", "two"}, {"one", "one"}} {
			if err := f(values); !errors.Is(err, ErrCardinality) {
				t.Fatal("ambiguous/missing field accepted", err)
			}
		}
	}
}

func TestCompactPreparseIsNotSignatureVerification(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	// A zero signature is intentionally not a valid cryptographic proof.
	parts := []string{enc([]byte(`{"alg":"EdDSA"}`)), enc([]byte(`{"not":"a health claim"}`)), enc(make([]byte, 64))}
	shape := "Bearer " + strings.Join(parts, ".")
	if err := CompactEncoding(shape); err != nil {
		t.Fatal("valid encoding rejected", err)
	}
	for _, bad := range []string{
		"Bearer " + strings.Repeat("A", 100), strings.Replace(shape, "Bearer ", "bearer ", 1),
		"Bearer " + parts[0] + "=." + parts[1] + "." + parts[2],
		"Bearer " + parts[0] + "\r\n." + parts[1] + "." + parts[2],
		"Bearer " + parts[0] + "." + parts[1] + "." + enc(make([]byte, 63)),
		"Bearer " + enc(make([]byte, 257)) + "." + parts[1] + "." + parts[2],
		"Bearer " + parts[0] + "." + enc(make([]byte, 12289)) + "." + parts[2],
		shape + ".extra", "Bearer SECRET_PRIVATE_MARKER",
		"Bearer " + parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-1] + "B",
	} {
		if err := CompactEncoding(bad); err == nil {
			t.Fatal("malformed/oversize encoding accepted")
		} else if strings.Contains(err.Error(), "SECRET_PRIVATE_MARKER") {
			t.Fatal("error echoed bearer material")
		}
	}
}
