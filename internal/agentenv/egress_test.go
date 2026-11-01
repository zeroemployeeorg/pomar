package agentenv

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEgressRefusalsDoNotDial(t *testing.T) {
	for _, test := range []struct{ name, method, host, ip string }{
		{"unlisted", "CONNECT", "elsewhere.example:443", "203.0.113.1"},
		{"port", "CONNECT", "allowed.example:80", "203.0.113.1"},
		{"method", "GET", "allowed.example:443", "203.0.113.1"},
		{"private", "CONNECT", "allowed.example:443", net.IPv4(10, 0, 0, 1).String()},
		{"loopback", "CONNECT", "allowed.example:443", "127.0.0.1"},
		{"shared space start", "CONNECT", "allowed.example:443", net.IPv4(100, 64, 0, 0).String()},
		{"tailnet", "CONNECT", "allowed.example:443", net.IPv4(100, 64, 1, 2).String()},
		{"shared space end", "CONNECT", "allowed.example:443", net.IPv4(100, 127, 255, 255).String()},
		{"mapped tailnet", "CONNECT", "allowed.example:443", net.IPv4(100, 64, 1, 2).To16().String()},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, _ := NewEgress([]string{"allowed.example"})
			g.Resolve = func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP(test.ip)}}, nil
			}
			g.Dial = func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("refused request dialed")
				return nil, nil
			}
			r := httptest.NewRequest(test.method, "http://allowed.example/", nil)
			r.Host = test.host
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("refused request returned %d", w.Code)
			}
		})
	}
}

func TestPublicAddressSharedSpaceBoundary(t *testing.T) {
	for _, address := range []string{net.IPv4(100, 63, 255, 255).String(), net.IPv4(100, 128, 0, 0).String(), "203.0.113.1"} {
		if !publicAddress(net.ParseIP(address)) {
			t.Errorf("public address adjacent to shared space refused: %s", address)
		}
	}
}

func TestEgressRejectsMixedPublicAndSharedDNSAnswers(t *testing.T) {
	for _, answers := range [][]string{{"203.0.113.1", net.IPv4(100, 64, 1, 2).String()}, {net.IPv4(100, 64, 1, 2).String(), "203.0.113.1"}} {
		g, err := NewEgress([]string{"allowed.example"})
		if err != nil {
			t.Fatal(err)
		}
		g.Resolve = func(context.Context, string) ([]net.IPAddr, error) {
			var ips []net.IPAddr
			for _, address := range answers {
				ips = append(ips, net.IPAddr{IP: net.ParseIP(address)})
			}
			return ips, nil
		}
		g.Dial = func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("mixed public/shared DNS answer dialed")
			return nil, nil
		}
		r := httptest.NewRequest("CONNECT", "http://allowed.example/", nil)
		r.Host = "allowed.example:443"
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("mixed DNS answer returned %d", w.Code)
		}
	}
}

func TestRevocationClosesExistingTunnelAndDeniesNew(t *testing.T) {
	g, _ := NewEgress([]string{"allowed.example"})
	g.Resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}, nil
	}
	peerChannel := make(chan net.Conn, 1)
	g.Dial = func(context.Context, string, string) (net.Conn, error) {
		up, peer := net.Pipe()
		peerChannel <- peer
		return up, nil
	}
	server := httptest.NewServer(g)
	defer server.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("tunnel: %v", err)
	}
	peer := <-peerChannel
	defer peer.Close()
	g.Revoke()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = r.ReadByte(); err == nil {
		t.Fatal("revoked client connection remained open")
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked upstream remained open")
	}
	w := httptest.NewRecorder()
	request := httptest.NewRequest("CONNECT", "http://allowed.example", nil)
	request.Host = "allowed.example:443"
	g.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal("new tunnel admitted after revocation")
	}
}

func TestPublicAddressRefusesBothSharedAddressRepresentations(t *testing.T) {
	ip := net.IPv4(100, 64, 1, 2)
	for _, candidate := range []net.IP{ip.To4(), ip.To16()} {
		if publicAddress(candidate) {
			t.Fatal("shared address representation accepted")
		}
	}
}
