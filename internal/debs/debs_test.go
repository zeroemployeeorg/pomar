package debs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// ar builds an ar archive the way dpkg-deb lays out a .deb.
func ar(members ...[2]string) []byte {
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", m[0], "0", "0", "0", "100644", len(m[1]))
		b.WriteString(m[1])
		if len(m[1])%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestArMember(t *testing.T) {
	deb := ar([2]string{"debian-binary", "2.0\n"}, [2]string{"control.tar.xz", "ctl"}, [2]string{"data.tar.xz", "DATA!"})
	got, err := arMember(deb, "data.tar.xz")
	if err != nil || string(got) != "DATA!" {
		t.Fatalf("arMember = %q, %v", got, err)
	}
	if _, err := arMember(deb, "data.tar.zst"); err == nil {
		t.Fatal("a missing member was found")
	}
	if _, err := arMember([]byte("not ar"), "x"); err == nil {
		t.Fatal("a non-archive was read")
	}
	bad := append([]byte(nil), deb...)
	copy(bad[8+48:8+58], "9999999999")
	if _, err := arMember(bad, "data.tar.xz"); err == nil {
		t.Fatal("an oversized member was accepted")
	}
}

func TestSetHashIsOrderIndependent(t *testing.T) {
	a := []Package{{SHA256: strings.Repeat("a", 64)}, {SHA256: strings.Repeat("b", 64)}}
	b := []Package{a[1], a[0]}
	if SetHash(a) != SetHash(b) || SetHash(a) == SetHash(a[:1]) {
		t.Fatal("SetHash is not a function of the set")
	}
}

func TestDataArchivesFetchVerifyAndCache(t *testing.T) {
	deb := ar([2]string{"debian-binary", "2.0\n"}, [2]string{"data.tar.xz", "payload"})
	var hits int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.URL.Path == "/tampered.deb" {
			w.Write(append(deb, 'x'))
			return
		}
		w.Write(deb)
	}))
	defer srv.Close()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	c := &Cache{Venue: v, Client: srv.Client()}
	good := Package{Name: "jq", URL: srv.URL + "/jq.deb", SHA256: sum(deb)}
	for i := 0; i < 2; i++ {
		paths, err := c.DataArchives(context.Background(), []Package{good})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(paths[0]); string(b) != "payload" {
			t.Fatalf("data archive = %q", b)
		}
	}
	if hits != 1 {
		t.Fatalf("fetched %d times, want once", hits)
	}
	// Pinned to a hash nothing cached has; the server sends other bytes.
	bad := Package{Name: "bad", URL: srv.URL + "/tampered.deb", SHA256: sum([]byte("the pinned bytes"))}
	if _, err := c.DataArchives(context.Background(), []Package{bad}); err == nil || !strings.Contains(err.Error(), "does not match the pin") {
		t.Fatalf("a tampered .deb was accepted: %v", err)
	}
	for _, p := range []Package{
		{Name: "http", URL: "http://example.invalid/x.deb", SHA256: sum(deb)},
		{Name: "short", URL: srv.URL + "/jq.deb", SHA256: "abc"},
	} {
		if _, err := c.DataArchives(context.Background(), []Package{p}); err == nil {
			t.Errorf("%s pin accepted", p.Name)
		}
	}
	if !v.IsOpen(venue.KindDownload, "debs") {
		t.Fatal("the package cache is not ledgered")
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("unaccounted: %v %v", un, err)
	}
}
