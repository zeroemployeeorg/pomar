package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

func TestHostMailChannelBindsSeatAndRevokesWithNetwork(t *testing.T) {
	root, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(root, "egress.sock")) > 103 {
		t.Fatal("use a short physical TMPDIR")
	}
	e := &Environment{Spec: VMSpec{Environment: "guest-1", Session: "session", Incarnation: "incarnation-1", Directory: root, EgressSocket: filepath.Join(root, "egress.sock"), HostConfig: HostConfig{Seat: "example-seat", MailOrigin: "http://mail.example:7743"}}}
	h := &Host{gates: map[string]*Egress{}, servers: map[string]*http.Server{}}
	if err := h.gate(e); err != nil {
		t.Fatal(err)
	}
	defer func() {
		h.gates[e.Spec.Environment].Revoke()
		for _, s := range h.servers {
			s.Close()
		}
	}()
	if e.Spec.MailSocket != filepath.Join(root, "mail.sock") {
		t.Fatal("mail socket not environment-bound")
	}
	fi, err := os.Lstat(e.Spec.MailSocket)
	if err != nil || fi.Mode().Perm() != 0600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatal("mail socket custody", fi, err)
	}
	// A transport may finish dialing asynchronously while replacement updates Spec.
	// Bind this client to the original channel rather than mutable owner state.
	mailSocket := e.Spec.MailSocket
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", mailSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	r, _ := http.NewRequest("GET", "http://local/v1/whoami", nil)
	r.Header.Set("X-Zmail-Instance", "other-seat")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("guest selected another seat", resp.StatusCode)
	}
	h.gates[e.Spec.Environment].Revoke()
	r.Header.Set("X-Zmail-Instance", "example-seat")
	resp, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal("network revoke left mail available", resp.StatusCode)
	}
	old := h.servers[e.Spec.Environment+"-mail"].Handler.(*mailRelay)
	e.Spec.Incarnation = "incarnation-2"
	if err := h.gate(e); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	old.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 503 {
		t.Fatal("replacement left predecessor relay usable", w.Code)
	}
}

func TestFailedMailChannelSetupClosesOtherChannels(t *testing.T) {
	root, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mail.sock"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	e := &Environment{Spec: VMSpec{Environment: "guest-1", Session: "session", Incarnation: "incarnation-1", Directory: root, EgressSocket: filepath.Join(root, "egress.sock"), HostConfig: HostConfig{Seat: "example-seat", MailOrigin: "http://mail.example:7743"}}}
	h := &Host{gates: map[string]*Egress{}, servers: map[string]*http.Server{}}
	if err := h.gate(e); err == nil {
		t.Fatal("occupied mail path replaced")
	}
	if len(h.gates) != 0 || len(h.servers) != 0 || e.Spec.MailSocket != "" {
		t.Fatal("failed setup left live channels")
	}
	b, _ := os.ReadFile(filepath.Join(root, "mail.sock"))
	if string(b) != "occupied" {
		t.Fatal("occupied mail path altered")
	}
}

type processFixture struct {
	processes []proc.Process
	err       error
}

func TestMailProfileMustOptInAndHaveASeat(t *testing.T) {
	h := &Host{config: HostConfig{MailOrigin: "http://mail.example:7743", Profiles: map[string]EnvironmentProfile{
		"disabled": {Seat: "example-seat"},
		"enabled":  {Seat: "example-seat", Mail: true},
	}}}
	for _, name := range []string{"disabled", "enabled"} {
		cfg, err := h.profileConfig(name)
		if err != nil {
			t.Fatal(err)
		}
		if (cfg.MailOrigin != "") != (name == "enabled") {
			t.Fatal("mail profile opt-in ignored", name)
		}
	}
	for _, p := range []EnvironmentProfile{{Mail: true}, {Mail: true, Seat: "example-seat"}} {
		_, err := OpenHost(HostConfig{Root: t.TempDir(), MaxLive: 1, CPUs: 1, MemoryBytes: 1 << 30, Profiles: map[string]EnvironmentProfile{"bad": p}})
		if err == nil {
			t.Fatal("unbound mail profile accepted")
		}
	}
}

func TestRetainedMailOriginCannotChangeOnRestart(t *testing.T) {
	e := &Environment{Phase: "created", Spec: VMSpec{Environment: "guest-1", Incarnation: "original", HostConfig: HostConfig{MailOrigin: "http://old.example:7743"}}}
	h := &Host{config: HostConfig{MailOrigin: "http://new.example:7743"}, envs: map[string]*Environment{}}
	if err := h.start(e); err == nil {
		t.Fatal("retained mail origin silently replaced")
	}
	if e.Spec.Incarnation != "original" || e.Phase != "created" {
		t.Fatal("mail refusal changed execution identity")
	}
}

func (p processFixture) List() ([]proc.Process, error) { return p.processes, p.err }

func TestStopUncertaintyDoesNotPermitReplacement(t *testing.T) {
	for _, test := range []struct {
		name               string
		observer           processFixture
		receiptIncarnation string
		confirmed          bool
	}{
		{"observer error", processFixture{err: errors.New("unknown inventory")}, "actor", false},
		{"old still alive", processFixture{processes: []proc.Process{{PID: 42, Start: "birth"}}}, "actor", false},
		{"stale receipt", processFixture{}, "predecessor", false},
		{"matching stop and departure", processFixture{}, "actor", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			e := &Environment{Spec: VMSpec{Environment: "env", Session: "session", Incarnation: "actor", Directory: dir}, Phase: "running", Process: proc.Process{PID: 42, Start: "birth"}, Actions: map[string]Action{}}
			data, _ := json.Marshal(map[string]string{"environment": e.Spec.Environment, "session": e.Spec.Session, "incarnation": test.receiptIncarnation, "vm_stopped": "true"})
			os.WriteFile(filepath.Join(dir, "vm-status.json"), data, 0o600)
			g, _ := NewEgress([]string{"allowed.example"})
			h := &Host{envs: map[string]*Environment{"env": e}, gates: map[string]*Egress{"env": g}, servers: map[string]*http.Server{}, processes: test.observer, stopWait: time.Millisecond}
			h.mu.Lock()
			err := h.stop(e)
			h.mu.Unlock()
			if (err == nil) != test.confirmed {
				t.Fatalf("stop: %+v %v", e, err)
			}
			if e.LastFence == nil || !e.LastFence.NetworkRevoked || !e.LastFence.AdapterAccessRevoked {
				t.Fatal("partial fence was not retained")
			}
			if e.LastFence.WorkspaceWriteRevoked != test.confirmed || e.LastFence.TerminationConfirmed != test.confirmed {
				t.Fatal("unknown execution claimed revoked")
			}
			if !test.confirmed {
				if err = h.start(e); err == nil {
					t.Fatal("replacement admitted before execution fence")
				}
			}
		})
	}
}

func TestRetainedMailSeatCannotChangeWithTheSameDeclarationHash(t *testing.T) {
	e := &Environment{Phase: "created", Spec: VMSpec{Environment: "guest-1", Profile: "seat", Incarnation: "original", HostConfig: HostConfig{Seat: "original-seat", SeatDeclarationSHA256: "same-digest", MailOrigin: "http://mail.example:7743"}}}
	h := &Host{config: HostConfig{MailOrigin: "http://mail.example:7743", Profiles: map[string]EnvironmentProfile{"seat": {Seat: "other-seat", SeatDeclarationSHA256: "same-digest", Mail: true}}}, envs: map[string]*Environment{}}
	if err := h.start(e); err == nil {
		t.Fatal("retained identity silently replaced")
	}
	if e.Spec.Incarnation != "original" || e.Spec.Seat != "original-seat" {
		t.Fatal("identity refusal changed retained state")
	}
}
