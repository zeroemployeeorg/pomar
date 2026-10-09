package agentenv

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A mounted owner handler is served beside the environment routes, and the
// environment routes are unchanged.
func TestMountServesAnOwnerHandlerBesideTheEnvironmentRoutes(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	cfg := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, SourceSHA: strings.Repeat("a", 40), SourceBundle: root + "/source.bundle", Base: root + "/base.ext4", ImageDigest: "sha256:" + strings.Repeat("b", 64)}
	cfg.ImageRef = "docker.io/library/golang@" + cfg.ImageDigest
	h, err := OpenHost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.Mount("/v1/seats", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/seats", nil))
	if w.Code != 299 {
		t.Fatalf("mounted route: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/environments/none", nil))
	if w.Code != 404 {
		t.Fatalf("environment route: %d %s", w.Code, w.Body.String())
	}
}
