// Package result writes and signs an attempt's result document (the CI record
// design's approach A): canonical JSON with sorted keys, written once at mode
// 0400 and never rewritten, and an ed25519 signature beside it.
//
// The signing key belongs to the role user the permanent manager runs as, in a
// mode-0700 directory of its data root (the signing-identity design's approach
// A). A manager running as an ordinary user holds no key: CheckRoleUser refuses
// it, so the stream's development manager can never sign a result.
package result

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Schema names the result document's format.
const Schema = "pomar.result/v1"

// Files written into an attempt's record, and the key's file name.
const (
	DocName = "result.json"
	SigName = "result.json.sig"
	keyName = "result-signing.key"
)

// FirstRegularUID is where macOS starts ordinary login accounts. Role users
// (hidden service accounts such as _pomar) sit below it.
const FirstRegularUID = 500

// ErrExists is returned when an attempt already has a result: it is written once.
var ErrExists = errors.New("result: already written")

// CheckRoleUser refuses signing for an ordinary account. The stream and every
// actor run as ordinary accounts; only the role user's manager signs.
func CheckRoleUser(uid int) error {
	if uid == 0 {
		return fmt.Errorf("result: refusing to sign as root; run the manager as its role user")
	}
	if uid >= FirstRegularUID {
		return fmt.Errorf("result: refusing to sign as uid %d, an ordinary account; only the role user's manager holds a key", uid)
	}
	return nil
}

// Signer holds the manager's result-signing key.
type Signer struct {
	priv ed25519.PrivateKey
	// ID is the key's fingerprint: the hex sha256 of its public key. The result
	// records it as its host id, so the public code names no host.
	ID string
}

// Public returns the key's public half, to publish and pin.
func (s *Signer) Public() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// KeyID is the fingerprint of a public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])
}

// LoadOrCreate loads the key in dir, creating it on first use. dir must exist,
// belong to uid and be mode 0700; the key file must belong to uid and be mode
// 0600. Anything else is refused rather than repaired.
func LoadOrCreate(dir string, uid int) (*Signer, error) {
	if err := owned(dir, uid, 0o700, true); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, keyName)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, gerr
		}
		f, oerr := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if oerr != nil {
			return nil, fmt.Errorf("result: %w", oerr)
		}
		_, werr := f.Write(priv.Seed())
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, fmt.Errorf("result: %w", werr)
		}
		b = priv.Seed()
	} else if err != nil {
		return nil, fmt.Errorf("result: %w", err)
	}
	if err := owned(p, uid, 0o600, false); err != nil {
		return nil, err
	}
	if len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("result: %s is not an ed25519 seed", p)
	}
	priv := ed25519.NewKeyFromSeed(b)
	return &Signer{priv: priv, ID: KeyID(priv.Public().(ed25519.PublicKey))}, nil
}

func owned(p string, uid int, mode fs.FileMode, dir bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if fi.IsDir() != dir || fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("result: %s is not a plain %s", p, map[bool]string{true: "directory", false: "file"}[dir])
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return fmt.Errorf("result: %s does not belong to uid %d", p, uid)
	}
	if fi.Mode().Perm() != mode {
		return fmt.Errorf("result: %s is mode %o, want %o", p, fi.Mode().Perm(), mode)
	}
	return nil
}

// Canonical encodes v as canonical JSON: object keys sorted at every level, no
// HTML escaping, no trailing newline, numbers kept as written.
func Canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil { // maps encode with sorted keys
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Sign signs a document.
func (s *Signer) Sign(doc []byte) []byte { return ed25519.Sign(s.priv, doc) }

// Verify checks a signature against a public key.
func Verify(pub ed25519.PublicKey, doc, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, doc, sig)
}

// Write puts the document, and its signature when there is one, into dir, each
// at mode 0400 and each only if it does not exist yet.
func Write(dir string, doc, sig []byte) error {
	if err := writeOnce(filepath.Join(dir, DocName), doc); err != nil {
		return err
	}
	if sig != nil {
		return writeOnce(filepath.Join(dir, SigName), sig)
	}
	return nil
}

func writeOnce(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s", ErrExists, p)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Read returns an attempt's document and signature (nil when unsigned).
func Read(dir string) (doc, sig []byte, err error) {
	doc, err = os.ReadFile(filepath.Join(dir, DocName))
	if err != nil {
		return nil, nil, err
	}
	sig, err = os.ReadFile(filepath.Join(dir, SigName))
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil, nil
	}
	return doc, sig, err
}
