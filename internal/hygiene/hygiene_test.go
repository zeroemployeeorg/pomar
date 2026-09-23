package hygiene

import (
	"strings"
	"testing"
)

// Test inputs that would themselves trip the checker are assembled at run
// time, so this file passes the check it tests.

func checker(t *testing.T, list string) *Checker {
	t.Helper()
	c, err := ParseDenyList(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseDenyListSkipsCommentsAndBlanks(t *testing.T) {
	c := checker(t, "# comment\n\n  Alpha \nbeta\n")
	if got := c.Entries(); got != 2 {
		t.Fatalf("Entries() = %d, want 2", got)
	}
}

func TestDenyListIsCaseInsensitiveAndWithheld(t *testing.T) {
	c := checker(t, "secret-host\n")
	fs := c.CheckFile("a.go", []byte("ok\nconnect to SECRET-HOST now\n"))
	if len(fs) != 1 || fs[0].Line != 2 || fs[0].Rule != "deny-list entry" {
		t.Fatalf("findings = %v", fs)
	}
	if fs[0].Match != "" || strings.Contains(fs[0].String(), "SECRET") {
		t.Fatalf("deny-list value leaked into report: %q", fs[0].String())
	}
}

func TestDenyListInPath(t *testing.T) {
	c := checker(t, "secret-host\n")
	fs := c.CheckFile("docs/secret-host.md", []byte("clean\n"))
	if len(fs) != 1 || fs[0].Rule != "deny-list entry in file path" {
		t.Fatalf("findings = %v", fs)
	}
}

func TestIPv4(t *testing.T) {
	c := checker(t, "")
	private := "10." + "20.30.40"
	cases := []struct {
		line string
		want int
	}{
		{"dial " + private + ":22", 1},
		{"listen on 127.0.0.1:8080", 0},
		{"example 192.0.2.10 and 203.0.113.5", 0},
		{"bind 0.0.0.0", 0},
		{"not an address 999.1.1.1", 0},
		{"version v" + private + "x", 0},
	}
	for _, tc := range cases {
		if got := len(c.CheckFile("x.txt", []byte(tc.line))); got != tc.want {
			t.Errorf("%q: %d findings, want %d", tc.line, got, tc.want)
		}
	}
}

func TestHomePath(t *testing.T) {
	c := checker(t, "")
	for _, p := range []string{"/Us" + "ers/alice/x", "/ho" + "me/bob"} {
		fs := c.CheckFile("x.sh", []byte("cd "+p+"\n"))
		if len(fs) != 1 || fs[0].Rule != "home-directory path" {
			t.Errorf("%q: findings = %v", p, fs)
		}
	}
	if fs := c.CheckFile("x.sh", []byte("cd $HOME/x\n")); len(fs) != 0 {
		t.Errorf("$HOME flagged: %v", fs)
	}
}

func TestBinarySkipped(t *testing.T) {
	c := checker(t, "secret\n")
	if fs := c.CheckFile("b.bin", []byte("secret\x00")); len(fs) != 0 {
		t.Fatalf("binary scanned: %v", fs)
	}
}
