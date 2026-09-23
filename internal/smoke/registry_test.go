package smoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func TestParseChallenge(t *testing.T) {
	c, err := parseChallenge(`Bearer realm="https://auth.example/token",service="reg.example",scope="repository:a/b:pull"`)
	if err != nil {
		t.Fatal(err)
	}
	if c["realm"] != "https://auth.example/token" || c["service"] != "reg.example" || c["scope"] != "repository:a/b:pull" {
		t.Fatalf("challenge = %v", c)
	}
	if _, err := parseChallenge("Basic realm=x"); err == nil {
		t.Fatal("basic challenge accepted")
	}
}

func TestRegistryHost(t *testing.T) {
	h, p, err := registryHost("docker.io/library/golang")
	if err != nil || h != "registry-1.docker.io" || p != "library/golang" {
		t.Fatalf("got %q %q %v", h, p, err)
	}
	if _, _, err := registryHost("golang"); err == nil {
		t.Fatal("reference without registry accepted")
	}
}

// A fake registry that demands a token, then serves a digest.
func fakeRegistry(t *testing.T, digest string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Write([]byte(`{"token":"t0k"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			if r.Header.Get("Authorization") != "Bearer t0k" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:x/y:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func TestCheckPinned(t *testing.T) {
	srv := fakeRegistry(t, "sha256:aaa")
	defer srv.Close()
	repo := strings.TrimPrefix(srv.URL, "https://") + "/x/y"
	ctx := context.Background()
	if err := CheckPinned(ctx, srv.Client(), repo, "1", "sha256:aaa"); err != nil {
		t.Fatalf("matching pin rejected: %v", err)
	}
	if err := CheckPinned(ctx, srv.Client(), repo, "1", "sha256:bbb"); err == nil {
		t.Fatal("moved tag accepted")
	}
}

func TestHostLedgersTmpBeforeUse(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Venue: v, HostBin: "/usr/bin/true"}
	if _, err := e.host(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := v.OpenObjects()
	if len(open) != 1 || open[0].ID != "tmp" || open[0].Last != venue.OpCreated || open[0].Class != venue.ClassCache {
		t.Fatalf("open = %+v, want the ledgered tmp cache", open)
	}
}
