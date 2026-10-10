package npmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The real handler may log from transport callbacks and parallel requests.
type traceBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *traceBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func (b *traceBuffer) snapshot() string {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.String()
}

func traceEvents(t *testing.T, data string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue // historical completion format remains available
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}

func TestTraceShowsReceivedAndMissBeforeUpstreamFinishes(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write(leftPad)
	}))
	defer up.Close()
	lock, err := ParseLock(strings.NewReader(lockJSON(t, 3, goodEntries(up.URL))), up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Lock: lock, Cache: t.TempDir(), Upstream: up.URL}
	var log traceBuffer
	srv := httptest.NewServer(p.Handler(&log))
	defer srv.Close()
	defer releaseOnce.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/left-pad/-/left-pad-1.3.0.tgz")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	<-entered
	events := traceEvents(t, log.snapshot())
	if len(events) < 2 || events[0]["event"] != "received" || events[1]["cache"] != "miss" {
		t.Fatalf("missing pre-completion observations: %v", events)
	}
	for _, e := range events {
		if e["event"] == "completed" {
			t.Fatal("upstream is still blocked")
		}
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A second request uses verified cache and creates no upstream stages.
	resp, err := http.Get(srv.URL + "/left-pad/-/left-pad-1.3.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	events = traceEvents(t, log.snapshot())
	firstByte, completed, hit := false, false, false
	for _, e := range events {
		if e["request"] == float64(1) {
			firstByte = firstByte || e["event"] == "upstream_first_byte"
			completed = completed || (e["event"] == "completed" && e["status"] == float64(200))
		} else if e["request"] == float64(2) {
			hit = hit || e["cache"] == "hit"
			if strings.HasPrefix(e["event"].(string), "upstream_") {
				t.Fatal("cache hit contacted upstream")
			}
		}
	}
	if !firstByte || !completed || !hit {
		t.Fatalf("missing first byte/completion/cache hit: %v", events)
	}
}

func TestTraceDoesNotExposeRefusedRequestData(t *testing.T) {
	p, up, _ := fixture(t, nil)
	defer up.Close()
	for _, target := range []string{"/unknown-secret-canary", "/left-pad/-/left-pad-1.3.0.tgz?token=secret-canary", "http://secret-canary.invalid/x"} {
		var log bytes.Buffer
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("Authorization", "secret-canary")
		p.Handler(&log).ServeHTTP(httptest.NewRecorder(), r)
		if strings.Contains(log.String(), "secret-canary") {
			t.Fatal("request data leaked into trace")
		}
	}
}
