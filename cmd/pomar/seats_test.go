package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/seatapi"
	"github.com/zeroemployeeorg/pomar/internal/seatdecl"
)

// seatHost serves the real seat routes on a host-shaped owner socket.
func seatHost(t *testing.T, mount bool) string {
	t.Helper()
	root, _ := os.MkdirTemp("/tmp", "seats")
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	store := seatapi.Store{Root: filepath.Join(root, "seats")}
	os.Mkdir(store.Root, 0o700)
	mux := http.NewServeMux()
	if mount {
		mux.Handle("/v1/seats", store.Handler())
		mux.Handle("/v1/seats/", store.Handler())
	}
	ln, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, "host.sock"), 0o600)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if mount {
		pin := strings.Repeat("c", 40)
		d := seatdecl.Declaration{Schema: seatdecl.Schema, Seat: "zeocreator", Project: "zeocreator",
			Sources: []seatdecl.Source{
				{Role: "work", Repository: "zeroemployeeorg/zeocreator", Fork: "architect/zeocreator", Branch: "main", Commit: pin, Tree: pin, Destination: "work", Writable: true},
				{Role: "records", Repository: "owner/records", Fork: "architect/records", Branch: "records/zeocreator", Commit: pin, Tree: pin, Destination: "records", Writable: true}},
			Class: "seat-linux-arm64-v1", Resources: seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
			Provider: seatdecl.Provider{Agent: "claude", Version: "2.1.280", Account: seatdecl.NotEstablished},
			Resume:   seatdecl.Resume{Policy: "fresh-from-sow", HandoverSOW: "x"},
			Identity: seatdecl.Identity{Role: seatdecl.NotEstablished, Forks: []string{"architect/zeocreator", "architect/records"},
				Push: []string{"refs/heads/zeocreator/*"}, Permissions: map[string]string{"contents": "write", "metadata": "read"}}}
		raw, _ := json.Marshal(d)
		if _, err := store.Declare("zeocreator", "", raw); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSeatsListsTheDeclaredSeats(t *testing.T) {
	root := seatHost(t, true)
	var out, errb bytes.Buffer
	if code := seatsCmd([]string{"-root", root}, nil, false, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "1 zeocreator NOT_ESTABLISHED NOT_ESTABLISHED none recorded rev 1") {
		t.Fatalf("list:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Pick a seat") {
		t.Fatal("a menu was offered off a terminal")
	}
}

func TestTheMenuSaysWhatIsntAvailableYet(t *testing.T) {
	root := seatHost(t, true)
	var out, errb bytes.Buffer
	if code := seatsCmd([]string{"-root", root}, strings.NewReader("1\n"), true, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "aren't available on this host yet") || !strings.Contains(out.String(), "pomar seat status zeocreator") {
		t.Fatalf("menu:\n%s", out.String())
	}
	if code := seatsCmd([]string{"-root", root}, strings.NewReader("7\n"), true, &out, &errb); code != 2 {
		t.Fatal("a bad number was accepted")
	}
}

func TestSeatStatusPrintsTheRecord(t *testing.T) {
	root := seatHost(t, true)
	var out, errb bytes.Buffer
	if code := seatCmd([]string{"status", "-root", root, "zeocreator"}, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	var got map[string]any
	if json.Unmarshal(out.Bytes(), &got) != nil || got["seat"] == nil || got["declaration"] == nil {
		t.Fatalf("status %s", out.String())
	}
	if code := seatCmd([]string{"status", "-root", root, "nobody"}, &out, &errb); code != 1 {
		t.Fatal("an undeclared seat was reported")
	}
}

func TestAHostWithoutSeatRoutesSaysSo(t *testing.T) {
	root := seatHost(t, false)
	var out, errb bytes.Buffer
	if code := seatsCmd([]string{"-root", root}, nil, false, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no seatRoot") {
		t.Fatalf("%d %s", code, errb.String())
	}
	if code := seatsCmd(nil, nil, false, &out, &errb); code != 1 {
		t.Fatal("no root was accepted")
	}
}
