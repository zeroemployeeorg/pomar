// Package healthreadlimits applies bounded pre-parse checks to health reads.
// Passing any check here is NOT authentication or authorization. The caller
// still needs the closed issuer profile, signature/trust/time/replay checks,
// independently registered resources and the actual transport's enforcement.
package healthreadlimits

import (
	"encoding/base64"
	"errors"
	"strings"
)

const (
	MaxResources     = 64
	MaxResourceBytes = 128
	MaxHeaderBytes   = 256
	MaxClaimsBytes   = 12288
	MaxLineBytes     = 16384
	MaxAuthBytes     = 20480
	MaxBlockBytes    = 32768
)

var (
	ErrSize        = errors.New("health read byte budget exceeded")
	ErrEncoding    = errors.New("health pass compact encoding refused")
	ErrResources   = errors.New("health resource shape refused")
	ErrEmptySet    = errors.New("health read empty resource set refused")
	ErrSetMismatch = errors.New("health resource sets differ")
	ErrCardinality = errors.New("health request field cardinality refused")
)

// RequestLine checks the byte budget only. The caller supplies the original
// HTTP/1 request line INCLUDING its terminating CRLF, not a reconstructed URL.
func RequestLine(raw []byte) error { return within(len(raw), MaxLineBytes) }

// HeaderBlock checks the byte budget only, INCLUDING the request line, field
// names, separators, every CRLF and the final blank line. This is not an HTTP
// parser or a net/http MaxHeaderBytes setting, nor an HTTP/2 qualification.
func HeaderBlock(raw []byte) error { return within(len(raw), MaxBlockBytes) }

// Authorization counts the whole value, including the seven Bearer bytes.
func Authorization(value string) error     { return within(len(value), MaxAuthBytes) }
func ProtectedHeader(decoded []byte) error { return within(len(decoded), MaxHeaderBytes) }
func Claims(decoded []byte) error          { return within(len(decoded), MaxClaimsBytes) }

func within(size, maximum int) error {
	if size > maximum {
		return ErrSize
	}
	return nil
}

// CompactEncoding refuses oversized/noncanonical segments before later JSON
// or cryptographic work. A well-encoded but unsigned, incorrectly signed or
// semantically invalid envelope can pass this check. Nothing returns a pass,
// principal, claims object or capability here; errors never echo input bytes.
func CompactEncoding(authorization string) error {
	if err := Authorization(authorization); err != nil {
		return err
	}
	if !strings.HasPrefix(authorization, "Bearer ") {
		return ErrEncoding
	}
	parts := strings.Split(authorization[len("Bearer "):], ".")
	if len(parts) != 3 {
		return ErrEncoding
	}
	for i, maximum := range []int{MaxHeaderBytes, MaxClaimsBytes, 64} {
		part := parts[i]
		if len(part) == 0 || len(part) > base64.RawURLEncoding.EncodedLen(maximum) {
			return ErrSize
		}
		// Go's decoder ignores CR/LF even in Strict mode. The compact format
		// forbids them, padding, whitespace and every non-base64url byte.
		for j := range part {
			c := part[j]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return ErrEncoding
			}
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(part)
		if err != nil {
			return ErrEncoding
		}
		if len(decoded) > maximum {
			return ErrSize
		}
		if i == 2 && len(decoded) != 64 {
			return ErrEncoding
		}
	}
	return nil
}

// SingleAuthorization and SingleIDs require the full collection from the
// parser. Header.Get/Query.Get discard duplicates before these guards can see
// them and must not be used as their input. Empty values fail subsequent
// encoding/resource checks; cardinality alone establishes no access.
func SingleAuthorization(values []string) error { return single(values) }
func SingleIDs(values []string) error           { return single(values) }
func single(values []string) error {
	if len(values) != 1 {
		return ErrCardinality
	}
	return nil
}
