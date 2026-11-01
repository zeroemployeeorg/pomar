package agentenv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mailTransport func(*http.Request) (*http.Response, error)

func (f mailTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mailResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestMailRelayFixesIdentityAndUpstream(t *testing.T) {
	var calls atomic.Int32
	m, err := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "mail.example:7743" || r.URL.Scheme != "http" || r.URL.Path != "/v1/send" {
			t.Errorf("unexpected upstream: %s", r.URL)
		}
		if r.Header.Get("X-Zmail-Instance") != "example-seat" || len(r.Header.Values("X-Zmail-Instance")) != 1 {
			t.Errorf("unbound identity: %v", r.Header)
		}
		for _, key := range []string{"Cookie", "Authorization", "Forwarded", "X-Forwarded-For"} {
			if r.Header.Get(key) != "" {
				t.Errorf("guest header forwarded: %s", key)
			}
		}
		return mailResponse(200, `{"seat":"example-seat"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(m)
	defer s.Close()
	r, _ := http.NewRequest("POST", s.URL+"/v1/send", strings.NewReader(`{"request_id":"one-send","to":"recipient","body":"hello"}`))
	r.Header.Set("X-Zmail-Instance", "example-seat")
	r.Header.Set("Content-Type", "application/json")
	for _, key := range []string{"Cookie", "Authorization", "Forwarded", "X-Forwarded-For"} {
		r.Header.Set(key, "guest-controlled")
	}
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("positive relay: status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestMailRelayRefusesOtherSeatAndUnlistedOperations(t *testing.T) {
	for _, c := range []struct{ method, path, identity, body string }{
		{"GET", "/v1/whoami", "other-seat", ""},
		{"GET", "/v1/seats", "", ""},
		{"GET", "/v1/export", "", ""},
		{"GET", "/v1/watch", "", ""},
		{"GET", "/v1/record/message", "", ""},
		{"GET", "/v1/codex-map", "", ""},
		{"GET", "/v1/waker", "", ""},
		{"GET", "/v1/client", "", ""},
		{"GET", "/v1/health", "", ""},
		{"GET", "/mcp", "", ""},
		{"POST", "/v1/send-group", "", `{}`},
		{"POST", "/v1/ack/id", "", `{}`},
		{"DELETE", "/v1/messages/id", "", ""},
		{"CONNECT", "/v1/whoami", "", ""},
		{"GET", "/v1/messages/a/b", "", ""},
		{"GET", "/v1/messages/%2e%2e", "", ""},
		{"GET", "/v1/inbox?seat=architect", "", ""},
		{"GET", "/v1/inbox?limit=1&limit=2", "", ""},
		{"GET", "/v1/inbox?limit=201", "", ""},
		{"GET", "/v1/inbox?all=1", "", ""},
		{"GET", "/v1/whoami?format=text", "", ""},
		{"POST", "/v1/send", "", `{"request_id":"id","sender":"other-seat"}`},
		{"POST", "/v1/send", "", `{"body":"no request id"}`},
		{"POST", "/v1/reply/id", "", `{"instance":"other-seat"}`},
		{"POST", "/v1/send", "", `{"request_id":"one","request_id":"two"}`},
		{"POST", "/v1/reply/id", "", `{"body":"one","body":"two"}`},
		{"POST", "/v1/reply/id", "", `{"to":"recipient","body":"no"}`},
		{"POST", "/v1/send", "", `{"request_id":"one","extra":true}`},
		{"POST", "/v1/send", "", `{"request_id":"one","\u017feat":"other-seat"}`},
		{"POST", "/v1/reply/id", "", `{"body":"one","bo\u0064y":"two"}`},
	} {
		t.Run(c.method+c.path+c.identity+c.body, func(t *testing.T) {
			m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) {
				t.Error("refused guest request reached upstream")
				return mailResponse(200, `{}`), nil
			}))
			s := httptest.NewServer(m)
			defer s.Close()
			r, _ := http.NewRequest(c.method, s.URL+c.path, strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			if c.identity != "" {
				r.Header.Set("X-Zmail-Instance", c.identity)
			}
			resp, err := s.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatalf("unlisted request accepted: %d", resp.StatusCode)
			}
		})
	}
}

func TestMailRelayReencodesReplyFields(t *testing.T) {
	m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != `{"body":"hello"}` {
			t.Errorf("raw reply keys or bytes forwarded: %q %v", b, err)
		}
		return mailResponse(200, `{}`), nil
	}))
	s := httptest.NewServer(m)
	defer s.Close()
	r, _ := http.NewRequest("POST", s.URL+"/v1/reply/id", strings.NewReader(`{ "bo\u0064y" : "hello" }`))
	r.Header.Set("Content-Type", "application/json")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("valid re-encoded reply refused", resp.StatusCode)
	}
}

func TestMailRelayQueriesAnd422BodyMatchContract(t *testing.T) {
	const payload = `{"exit":1,"output":"owner refusal reason"}`
	for _, target := range []string{"/v1/whoami?format=json", "/v1/inbox?limit=0&format=json", "/v1/pending?limit=200&format=json", "/v1/messages/message-id?format=json"} {
		m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != target {
				t.Errorf("query changed: got %s want %s", r.URL.RequestURI(), target)
			}
			return mailResponse(422, payload), nil
		}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 422 || w.Body.String() != payload {
			t.Fatalf("422 diagnostic not preserved: %d %q", w.Code, w.Body.String())
		}
	}
}

func TestMailRelayRetryKeyIsGuestScopedAndStableAcrossIncarnations(t *testing.T) {
	var keys []string
	for _, c := range []struct{ seat, guest, incarnation string }{{"example-seat", "guest-1", "incarnation-1"}, {"example-seat", "guest-1", "incarnation-2"}, {"example-seat", "guest-2", "incarnation-1"}, {"other-seat", "guest-1", "incarnation-1"}} {
		m, _ := newMailRelay(c.seat, c.guest, c.incarnation, "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, body["request_id"].(string))
			return mailResponse(200, `{}`), nil
		}))
		s := httptest.NewServer(m)
		r, _ := http.NewRequest("POST", s.URL+"/v1/send", strings.NewReader(`{"request_id":"same-retry-key","to":"recipient","body":"hello"}`))
		r.Header.Set("Content-Type", "application/json")
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		s.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("send refused: %d", resp.StatusCode)
		}
	}
	if len(keys) != 4 || len(keys[0]) != 64 || keys[0] != keys[1] || keys[0] == keys[2] || keys[0] == keys[3] || keys[2] == keys[3] {
		t.Fatalf("guest retry namespace incorrect: %v", keys)
	}
}

func TestMailRelayOneMiBRequestLimitAndOriginRefusal(t *testing.T) {
	var calls atomic.Int32
	m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if len(b) != mailRequestLimit {
			t.Errorf("body truncated or changed: %d", len(b))
		}
		return mailResponse(200, `{}`), nil
	}))
	s := httptest.NewServer(m)
	defer s.Close()
	for _, c := range []struct {
		extra         int
		header, value string
	}{{0, "", ""}, {1, "", ""}, {0, "Origin", "http://guest.example"}, {0, "Sec-Fetch-Site", "same-origin"}, {0, "Sec-Fetch-Mode", "cors"}, {0, "Content-Type", "text/plain"}} {
		body := `{"body":"` + strings.Repeat("x", mailRequestLimit-11+c.extra) + `"}`
		r, _ := http.NewRequest("POST", s.URL+"/v1/reply/id", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if c.extra == 0 && c.header == "" && resp.StatusCode != 200 || (c.extra != 0 || c.header != "") && resp.StatusCode < 400 {
			t.Fatalf("limit/header refusal incorrect: %+v status=%d", c, resp.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refused body or origin reached upstream: %d calls", calls.Load())
	}
}

func TestMailRelayDoesNotFollowRedirectOrForwardOversizeBody(t *testing.T) {
	for _, code := range []int{302, 200} {
		var calls atomic.Int32
		m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			r := mailResponse(code, strings.Repeat("x", mailResponseLimit+1))
			r.Header.Set("Location", "http://other.example/steal")
			r.Header.Set("Set-Cookie", "secret=value")
			return r, nil
		}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
		if calls.Load() != 1 || w.Code != 502 || w.Header().Get("Location") != "" || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("unsafe response escaped: code=%d calls=%d headers=%v", w.Code, calls.Load(), w.Header())
		}
	}
}

func TestMailRelayRevokesInflightAndFutureRequests(t *testing.T) {
	entered := make(chan struct{})
	m, _ := newMailRelay("example-seat", "guest-1", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/whoami", nil))
	}()
	<-entered
	m.Revoke()
	<-done
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 503 {
		t.Fatalf("revoked relay accepted another request: %d", w.Code)
	}
}

func TestMailRelayWriteBudgetRefillsAndLeavesReadsIndependent(t *testing.T) {
	var calls atomic.Int32
	m, err := newMailRelay("example-seat", "guest-rate", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) { calls.Add(1); return mailResponse(200, `{}`), nil }))
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	base := time.Now()
	clock.Store(base.UnixNano())
	m.writeRefill = base
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }
	server := httptest.NewServer(m)
	defer server.Close()
	post := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+path, strings.NewReader(`{"body":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	for i := 0; i < 8; i++ {
		if code, _ := post("/v1/reply/message-one"); code != 200 {
			t.Fatalf("burst %d: %d", i, code)
		}
	}
	if code, retry := post("/v1/reply/message-one"); code != 429 || retry != "2" || calls.Load() != 8 {
		t.Fatalf("exhaustion %d %s calls%d", code, retry, calls.Load())
	}
	resp, err := server.Client().Get(server.URL + "/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 9 {
		t.Fatal("write exhaustion blocked read")
	}
	clock.Store(base.Add(2 * time.Second).UnixNano())
	if code, _ := post("/v1/reply/message-one"); code != 200 {
		t.Fatalf("refill %d", code)
	}
	if code, _ := post("/v1/reply/message-one"); code != 429 || calls.Load() != 10 {
		t.Fatal("refill granted more than one write")
	}
	clock.Store(base.Add(time.Hour).UnixNano())
	for i := 0; i < 8; i++ {
		if code, _ := post("/v1/reply/message-one"); code != 200 {
			t.Fatal("idle refill failed")
		}
	}
	if code, _ := post("/v1/reply/message-one"); code != 429 {
		t.Fatal("idle accumulated beyond burst")
	}
}

func TestMailRelayAllGuestsShareUpstreamConcurrency(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	done := make(chan struct{}, 4)
	defer close(release)
	for i := 0; i < 4; i++ {
		m, err := newMailRelay("example-seat", fmt.Sprintf("guest-%d", i), "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return mailResponse(200, `{}`), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			defer func() { done <- struct{}{} }()
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/whoami", nil))
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not start")
		}
	}
	var calls atomic.Int32
	m, err := newMailRelay("example-seat", "guest-fifth", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) { calls.Add(1); return mailResponse(200, `{}`), nil }))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || calls.Load() != 0 {
		t.Fatalf("shared bound %d %v calls%d", w.Code, w.Header(), calls.Load())
	}
	// Release every original before testing that the shared slots are reusable.
	for i := 0; i < 4; i++ {
		release <- struct{}{}
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not drain")
		}
	}
	w = httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal("slot leaked")
	}
}

func TestMailRelayPassesUpstreamBusyWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	m, err := newMailRelay("example-seat", "guest-busy", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := mailResponse(503, `{"exit":1,"output":"busy"}`)
		resp.Header.Set("Retry-After", "7")
		return resp, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "7" || calls.Load() != 1 || w.Body.String() != `{"exit":1,"output":"busy"}` {
		t.Fatalf("busy changed/retried: %d %v %q calls%d", w.Code, w.Header(), w.Body.String(), calls.Load())
	}
}

type blockedMailWriter struct {
	header  http.Header
	entered chan struct{}
	release <-chan struct{}
}

func (w *blockedMailWriter) Header() http.Header { return w.header }
func (w *blockedMailWriter) WriteHeader(int)     {}
func (w *blockedMailWriter) Write(b []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.release
	return len(b), nil
}

func TestMailRelaySlowGuestDoesNotHoldSharedUpstream(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{}, 4)
	entered := make(chan struct{}, 4)
	defer func() {
		close(release)
		for i := 0; i < 4; i++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("slow writer did not drain")
			}
		}
	}()
	for i := 0; i < 4; i++ {
		m, err := newMailRelay("example-seat", fmt.Sprintf("slow-%d", i), "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) { return mailResponse(200, `{}`), nil }))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			defer func() { done <- struct{}{} }()
			m.ServeHTTP(&blockedMailWriter{header: http.Header{}, entered: entered, release: release}, httptest.NewRequest("GET", "/v1/whoami", nil))
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not reach response")
		}
	}
	m, err := newMailRelay("example-seat", "healthy-guest", "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) { return mailResponse(200, `{}`), nil }))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 200 {
		t.Fatalf("slow guests exhausted upstream: %d", w.Code)
	}
}

func TestMailRelayOneGuestCannotHoldAllUpstreamSlots(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	done := make(chan struct{}, 2)
	m, err := newMailRelay("example-seat", "one-reader", "incarnation-1", "http://mail.example:7743", mailTransport(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return mailResponse(200, `{}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		for i := 0; i < 2; i++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("reader did not drain")
			}
		}
	}()
	for i := 0; i < 2; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/whoami", nil))
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("read did not enter")
		}
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 503 {
		t.Fatalf("guest exceeded share: %d", w.Code)
	}
	other, err := newMailRelay("example-seat", "other-reader", "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) { return mailResponse(200, `{}`), nil }))
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	other.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
	if w.Code != 200 {
		t.Fatal("one guest denied another's share")
	}
}

func TestMailRelayRetryHeaders(t *testing.T) {
	for _, tc := range []struct {
		code           int
		supplied, want string
	}{{503, "", "5"}, {503, "7", "7"}, {429, "11", "11"}} {
		t.Run(fmt.Sprintf("%d-%s", tc.code, tc.supplied), func(t *testing.T) {
			var calls atomic.Int32
			m, err := newMailRelay("example-seat", "retry-guest", "incarnation-1", "http://mail.example:7743", mailTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				resp := mailResponse(tc.code, `{}`)
				if tc.supplied != "" {
					resp.Header.Set("Retry-After", tc.supplied)
				}
				return resp, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/whoami", nil))
			if w.Code != tc.code || w.Header().Get("Retry-After") != tc.want || calls.Load() != 1 {
				t.Fatalf("retry contract %d %v calls%d", w.Code, w.Header(), calls.Load())
			}
		})
	}
}
