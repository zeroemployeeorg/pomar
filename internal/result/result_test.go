package result

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRoleUserRefusesOrdinaryAccountsAndRoot(t *testing.T) {
	for _, uid := range []int{0, 500, 501, 503} {
		if CheckRoleUser(uid) == nil {
			t.Fatalf("uid %d allowed to sign", uid)
		}
	}
	if err := CheckRoleUser(410); err != nil {
		t.Fatalf("a role user (410) refused: %v", err)
	}
}

func TestKeyIsCreatedOnceThenLoaded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := LoadOrCreate(dir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, keyName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, mode %v; want 0600", err, fi.Mode().Perm())
	}
	b, err := LoadOrCreate(dir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(a.ID) != 64 {
		t.Fatalf("the key changed or has no id: %q then %q", a.ID, b.ID)
	}
}

func TestKeyDirectoryAndFileMustBeTheOwnersAlone(t *testing.T) {
	open := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(open, 0o755)
	if _, err := LoadOrCreate(open, os.Getuid()); err == nil || !strings.Contains(err.Error(), "want 700") {
		t.Fatalf("an open key directory: %v, want a refusal", err)
	}
	dir := filepath.Join(t.TempDir(), "keys")
	os.Mkdir(dir, 0o700)
	if _, err := LoadOrCreate(dir, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("a key directory of another uid: %v, want a refusal", err)
	}
	if _, err := LoadOrCreate(dir, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, keyName), 0o644)
	if _, err := LoadOrCreate(dir, os.Getuid()); err == nil || !strings.Contains(err.Error(), "want 600") {
		t.Fatalf("a readable key file: %v, want a refusal", err)
	}
}

func TestCanonicalSortsKeysAtEveryLevel(t *testing.T) {
	type inner struct {
		Z int `json:"z"`
		A int `json:"a"`
	}
	b, err := Canonical(map[string]any{"b": inner{Z: 1, A: 2}, "a": "<&>", "n": int64(1) << 40})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"<&>","b":{"a":2,"z":1},"n":1099511627776}`
	if string(b) != want {
		t.Fatalf("canonical:\n got %s\nwant %s", b, want)
	}
}

func TestSignedResultVerifiesAndATamperedOneDoesNot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	os.Mkdir(dir, 0o700)
	s, err := LoadOrCreate(dir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := Canonical(map[string]any{"state": "exited", "exit_code": 0})
	rec := t.TempDir()
	if err := Write(rec, doc, s.Sign(doc)); err != nil {
		t.Fatal(err)
	}
	gotDoc, gotSig, err := Read(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(s.Public(), gotDoc, gotSig) {
		t.Fatal("a fresh signed result does not verify")
	}
	forged := []byte(strings.Replace(string(gotDoc), `"exit_code":0`, `"exit_code":1`, 1))
	if Verify(s.Public(), forged, gotSig) {
		t.Fatal("a changed result verifies")
	}
	fi, _ := os.Stat(filepath.Join(rec, DocName))
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("result mode %o, want 400", fi.Mode().Perm())
	}
	if err := Write(rec, doc, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("a second write: %v, want ErrExists", err)
	}
}
