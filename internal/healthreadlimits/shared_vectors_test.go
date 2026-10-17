package healthreadlimits

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
)

// The independent synthetic packet is supplied privately by maintainers, not
// copied out of their records into this repository. Its immutable manifest
// binds the original 29 cases. Public tests above need no external packet.
func TestSharedVectorPreparseGuards(t *testing.T) {
	dir := os.Getenv("POMAR_HEALTH_VECTOR_DIR")
	if dir == "" {
		t.Skip("independent shared vector packet not supplied")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	read := func(t *testing.T, name string) []byte {
		t.Helper()
		f, err := root.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
		if err != nil || len(b) > 128<<10 {
			t.Fatal("vector file exceeds intake bound")
		}
		return b
	}
	digest := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	manifest := read(t, "manifest.json")
	if digest(manifest) != "e2649210b7fb8da54972e1cea2bbcd29413dad548011d090fae0fba68a2738b4" {
		t.Fatal("independent manifest digest mismatch")
	}
	var packet struct {
		Cases []struct {
			Name         string `json:"name"`
			File         string `json:"file"`
			Bytes        int    `json:"bytes"`
			SHA256       string `json:"sha256"`
			Layer        string `json:"layer"`
			Expectation  string `json:"expectation"`
			Encoding     string `json:"storage_encoding"`
			StoredSHA256 string `json:"stored_sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(manifest, &packet); err != nil || len(packet.Cases) != 29 {
		t.Fatal("invalid independent packet")
	}
	for _, tc := range packet.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if !strings.HasPrefix(tc.File, "fixtures/") || path.Clean(tc.File) != tc.File || strings.Contains(tc.File, "..") {
				t.Fatal("vector path refused")
			}
			stored := read(t, tc.File)
			data := stored
			if tc.Encoding == "base64" {
				if digest(stored) != tc.StoredSHA256 {
					t.Fatal("stored vector digest mismatch")
				}
				data, err = base64.StdEncoding.Strict().DecodeString(string(stored))
				if err != nil {
					t.Fatal("vector storage encoding")
				}
			} else if tc.Encoding != "" {
				t.Fatal("unsupported vector storage")
			}
			if len(data) != tc.Bytes || digest(data) != tc.SHA256 {
				t.Fatal("decoded vector bytes/digest mismatch")
			}
			var guardErr error
			switch tc.Layer {
			case "resource-shape":
				var ids []string
				if json.Unmarshal(data, &ids) != nil {
					t.Fatal("resource vector shape")
				}
				guardErr = Resources(ids)
				if tc.Expectation == "review-empty-set-policy" && !errors.Is(guardErr, ErrEmptySet) {
					t.Fatal("empty set did not hold")
				}
			case "set-binding":
				var sets struct {
					Authorised []string `json:"authorized"`
					Request    []string `json:"request"`
				}
				if json.Unmarshal(data, &sets) != nil {
					t.Fatal("set vector shape")
				}
				guardErr = SameResources(sets.Authorised, sets.Request)
			case "claims-preparse-budget":
				guardErr = Claims(data)
			case "protected-header-preparse-budget":
				guardErr = ProtectedHeader(data)
			case "authorization-preparse-budget":
				guardErr = Authorization(string(data))
			case "http1-request-line-budget-proposal":
				guardErr = RequestLine(data)
			case "http1-whole-header-block-budget-proposal":
				guardErr = HeaderBlock(data)
			case "http1-composed-wire", "http1-header-cardinality", "query-cardinality":
				req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(data)))
				if err != nil {
					t.Fatal("synthetic HTTP input parse failed", err)
				}
				defer req.Body.Close()
				values, err := url.ParseQuery(req.URL.RawQuery)
				if err != nil {
					t.Fatal("synthetic query parse failed")
				}
				switch tc.Layer {
				case "http1-header-cardinality":
					guardErr = SingleAuthorization(req.Header.Values("Authorization"))
				case "query-cardinality":
					guardErr = SingleIDs(values["ids"])
				default:
					if HeaderBlock(data) != nil {
						t.Fatal("composed block over budget")
					}
					line, _, found := bytes.Cut(data, []byte("\r\n"))
					if !found || RequestLine(append(line, '\r', '\n')) != nil {
						t.Fatal("composed line over budget")
					}
					if Authorization(req.Header.Get("Authorization")) != nil {
						t.Fatal("composed authorization over budget")
					}
					if CompactEncoding(req.Header.Get("Authorization")) == nil {
						t.Fatal("placeholder became a usable compact shape")
					}
					// This equality case tests budgets only. The placeholder is
					// deliberately unusable; no signature acceptance is claimed.
				}
			default:
				t.Fatal("unhandled independent vector layer")
			}
			withinBudget := strings.HasPrefix(tc.Expectation, "within-") || tc.Expectation == "equal-set"
			if withinBudget && guardErr != nil {
				t.Fatal("within-budget/equal-set guard refused", guardErr)
			}
			if !withinBudget && guardErr == nil {
				t.Fatal("refusal vector passed its guard")
			}
		})
	}
	// ReadRequest above is a fixture parser, not a running HTTP server, a
	// MaxHeaderBytes receipt, HTTP/2 test or signature/trust/replay positive.
}
