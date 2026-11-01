package agentenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mailRequestLimit = 1 << 20
const mailResponseLimit = 1 << 20

// All relay instances in this owner process share the node's upstream budget.
// A slow response holds its slot until its bounded body has been received.
var mailUpstreamSlots = make(chan struct{}, 4)

// mailRelay is bound by the owner to one guest incarnation. It is not a
// general proxy. A dedicated local channel and Revoke on stop/replacement
// are required before this component can be integrated into a host.
type mailRelay struct {
	seat, guest, incarnation string
	upstream                 *url.URL
	client                   *http.Client
	mu                       sync.Mutex
	revoked                  bool
	inflight                 map[*http.Request]context.CancelFunc
	writeTokens              int
	writeRefill              time.Time
	now                      func() time.Time
}

var mailID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// upstream comes from the mail service owner's API contract, never a guest.
// transport is an owner/test dependency; nil uses direct HTTP, without an
// environment-selected proxy. No credential or guest header is inherited.
func newMailRelay(seat, guest, incarnation, upstream string, transport http.RoundTripper) (*mailRelay, error) {
	u, err := url.Parse(upstream)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("mail relay needs a fixed owner upstream")
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || host == "" || strings.ToLower(host) != host || !strings.Contains(host, ".") || strings.ContainsAny(host, "/*:@ ") || net.ParseIP(host) != nil || !seatProfileName.MatchString(seat) || !validID.MatchString(guest) || !validID.MatchString(incarnation) || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("mail relay needs an owner-bound seat, guest, incarnation and upstream origin")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if transport == nil {
		transport = &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 5 * time.Second}
	}
	return &mailRelay{seat: seat, guest: guest, incarnation: incarnation, upstream: u, inflight: map[*http.Request]context.CancelFunc{}, writeTokens: 8, writeRefill: time.Now(), now: time.Now, client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (m *mailRelay) Revoke() {
	m.mu.Lock()
	m.revoked = true
	for _, cancel := range m.inflight {
		cancel()
	}
	m.mu.Unlock()
	m.client.CloseIdleConnections()
}

func mailRoute(r *http.Request) bool {
	if r.URL.IsAbs() || r.URL.User != nil || r.URL.Fragment != "" || strings.Contains(r.URL.EscapedPath(), "%") || path.Clean(r.URL.Path) != r.URL.Path || strings.HasPrefix(r.URL.Path, "//") {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		return false
	}
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	allowed := r.Method == "GET" && (p == "/whoami" || p == "/inbox" || p == "/pending") || r.Method == "POST" && p == "/send"
	for _, route := range []struct{ method, prefix string }{{"GET", "/messages/"}, {"POST", "/reply/"}} {
		if r.Method == route.method && strings.HasPrefix(p, route.prefix) && mailID.MatchString(strings.TrimPrefix(p, route.prefix)) {
			allowed = true
		}
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !allowed {
		return false
	}
	for key, values := range q {
		if r.Method != "GET" || len(values) != 1 {
			return false
		}
		if key == "format" && values[0] == "json" {
			continue
		}
		if key != "limit" || (p != "/inbox" && p != "/pending") {
			return false
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 || n > 200 || strconv.Itoa(n) != values[0] {
			return false
		}
	}
	return true
}

func strictMailBody(raw []byte, send bool) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("mail body must be an object")
	}
	object := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		allowed := key == "body" || key == "priority" || key == "final" || key == "json" || send && (key == "to" || key == "subject" || key == "request_id")
		if err != nil || !ok || !allowed || object[key] != nil {
			return nil, errors.New("unknown or duplicate mail field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		object[key] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid mail object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing mail data")
	}
	return object, nil
}

func (m *mailRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identities := r.Header.Values("X-Zmail-Instance")
	if !mailRoute(r) || len(identities) > 1 || len(identities) == 1 && identities[0] != m.seat || len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Sec-Fetch-Site")) > 0 || len(r.Header.Values("Sec-Fetch-Mode")) > 0 || r.Header.Get("Upgrade") != "" || len(r.TransferEncoding) != 0 || r.ContentLength < 0 || r.ContentLength > mailRequestLimit || r.Method == "GET" && r.ContentLength != 0 || r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "mail relay refused", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m.mu.Lock()
	if m.revoked || len(m.inflight) >= 4 {
		m.mu.Unlock()
		http.Error(w, "mail relay unavailable", http.StatusServiceUnavailable)
		return
	}
	m.inflight[r] = cancel
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.inflight, r); m.mu.Unlock() }()
	var body []byte
	if r.Method == "POST" {
		// Bound a slow local caller as well as the upstream. A transport
		// without read-deadline support is not an admitted relay channel.
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			http.Error(w, "mail channel unavailable", http.StatusServiceUnavailable)
			return
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, mailRequestLimit+1))
		if err != nil || len(body) > mailRequestLimit {
			http.Error(w, "mail body refused", http.StatusBadRequest)
			return
		}
		send := r.URL.Path == "/v1/send"
		object, err := strictMailBody(body, send)
		if err != nil {
			http.Error(w, "mail fields refused", http.StatusBadRequest)
			return
		}
		if send {
			var id string
			if json.Unmarshal(object["request_id"], &id) != nil || strings.TrimSpace(id) == "" || len(id) > 4096 {
				http.Error(w, "mail request_id required", http.StatusBadRequest)
				return
			}
			// A guest retry key is stable across incarnation changes but
			// distinct from every other guest's key. No guest chooses the
			// owner-side namespace, and no retry is silently re-keyed.
			sum := sha256.Sum256([]byte(m.seat + "\x00" + m.guest + "\x00" + id))
			object["request_id"], _ = json.Marshal(hex.EncodeToString(sum[:]))
		}
		// Re-encode every permitted object, including replies. Raw guest
		// keys and duplicate/Unicode-fold variants never reach upstream.
		body, _ = json.Marshal(object)
		if len(body) > mailRequestLimit {
			http.Error(w, "scoped mail body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}
	// Local admission never waits for shared capacity and never retries upstream.
	select {
	case mailUpstreamSlots <- struct{}{}:
		defer func() { <-mailUpstreamSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "mail upstream busy", http.StatusServiceUnavailable)
		return
	}
	if r.Method == "POST" {
		m.mu.Lock()
		now := m.now()
		if refill := int(now.Sub(m.writeRefill) / (2 * time.Second)); refill > 0 {
			m.writeTokens = min(8, m.writeTokens+refill)
			m.writeRefill = m.writeRefill.Add(time.Duration(refill) * 2 * time.Second)
		}
		if m.writeTokens == 0 {
			wait := 2*time.Second - now.Sub(m.writeRefill)
			seconds := max(1, int((wait+time.Second-1)/time.Second))
			m.mu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			http.Error(w, "mail write rate exceeded", http.StatusTooManyRequests)
			return
		}
		m.writeTokens--
		m.mu.Unlock()
	}
	u := *m.upstream
	u.Path += r.URL.Path
	u.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "mail request refused", http.StatusBadRequest)
		return
	}
	out.Header.Set("X-Zmail-Instance", m.seat)
	if r.Method == "POST" {
		out.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(out)
	if err != nil {
		http.Error(w, "mail upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, mailResponseLimit+1))
	m.mu.Lock()
	revoked := m.revoked
	m.mu.Unlock()
	if err != nil || len(b) > mailResponseLimit || resp.StatusCode >= 300 && resp.StatusCode < 400 || revoked {
		http.Error(w, "mail upstream refused", http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != "" {
		w.Header().Set("Retry-After", resp.Header.Get("Retry-After"))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(b)
}
