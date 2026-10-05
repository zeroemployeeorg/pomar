package agentenv

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Egress is one environment's revocable network lease. It tunnels TLS without
// inspecting credentials. No arbitrary HTTP, DNS, IP literal or private endpoint
// is exposed to the guest. Revocation closes both sides of every live tunnel.
type Egress struct {
	mu          sync.Mutex
	revoked     bool
	allowed     map[string]bool
	connections map[net.Conn]struct{}
	Resolve     func(context.Context, string) ([]net.IPAddr, error)
	Dial        func(context.Context, string, string) (net.Conn, error)
}

func NewEgress(names []string) (*Egress, error) {
	g := &Egress{allowed: map[string]bool{}, connections: map[net.Conn]struct{}{}}
	for _, name := range names {
		if name != strings.ToLower(name) || strings.ContainsAny(name, "/*:@ ") || !strings.Contains(name, ".") || net.ParseIP(name) != nil {
			return nil, errors.New("egress needs exact lower-case DNS names")
		}
		g.allowed[name] = true
	}
	g.Resolve = net.DefaultResolver.LookupIPAddr
	g.Dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	return g, nil
}

func publicAddress(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

func (g *Egress) Revoke() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked = true
	for c := range g.connections {
		c.Close()
	}
}

func (g *Egress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	g.mu.Lock()
	allowed := !g.revoked && g.allowed[host] && len(g.connections) < 64
	g.mu.Unlock()
	if err != nil || r.Method != http.MethodConnect || port != "443" || !allowed {
		http.Error(w, "egress refused", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ips, err := g.Resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		http.Error(w, "egress resolution unavailable", 502)
		return
	}
	for _, ip := range ips {
		if !publicAddress(ip.IP) {
			http.Error(w, "egress refused", 403)
			return
		}
	}
	upstream, err := g.Dial(ctx, "tcp", net.JoinHostPort(ips[0].IP.String(), "443"))
	if err != nil {
		http.Error(w, "egress unavailable", 502)
		return
	}
	defer upstream.Close()
	// Check the lease again after DNS/dial, before admitting the tunnel.
	g.mu.Lock()
	if g.revoked || len(g.connections) >= 64 {
		g.mu.Unlock()
		http.Error(w, "egress revoked", 403)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		g.mu.Unlock()
		http.Error(w, "tunnel unavailable", 500)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		g.mu.Unlock()
		return
	}
	g.connections[client] = struct{}{}
	g.connections[upstream] = struct{}{}
	g.mu.Unlock()
	defer func() {
		client.Close()
		g.mu.Lock()
		delete(g.connections, client)
		delete(g.connections, upstream)
		g.mu.Unlock()
	}()
	// Connections are bounded even when a remote endpoint never responds.
	deadline := time.Now().Add(15 * time.Minute)
	client.SetDeadline(deadline)
	upstream.SetDeadline(deadline)
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = rw.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(upstream, rw); upstream.Close(); close(done) }()
	io.Copy(client, upstream)
	client.Close()
	<-done
}
