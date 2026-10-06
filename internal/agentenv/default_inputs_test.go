package agentenv

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultEnvironmentRefusesChangedInputsBeforeRewritingItsIdentity(t *testing.T) {
	for _, field := range []string{"source_sha", "source_bundle", "base", "image_digest", "image_ref"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, SourceSHA: strings.Repeat("a", 40), SourceBundle: filepath.Join(root, "source.bundle"), Base: filepath.Join(root, "base.ext4"), ImageDigest: "sha256:" + strings.Repeat("b", 64)}
			cfg.ImageRef = "docker.io/library/golang@" + cfg.ImageDigest
			h, err := OpenHost(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments", strings.NewReader(`{"id":"default","operation_id":"create-default"}`)))
			if w.Code != 201 {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			e := h.envs["default"]
			if e.Spec.Profile != "" {
				t.Fatal("fixture selected a named profile")
			}
			before, _ := json.Marshal(e)
			record := filepath.Join(e.Spec.Directory, "environment.json")
			diskBefore, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "source_sha":
				h.config.SourceSHA = strings.Repeat("c", 40)
			case "source_bundle":
				h.config.SourceBundle = filepath.Join(root, "changed.bundle")
			case "base":
				h.config.Base = filepath.Join(root, "changed.ext4")
			case "image_digest":
				h.config.ImageDigest = "sha256:" + strings.Repeat("d", 64)
			case "image_ref":
				h.config.ImageRef = "docker.io/library/python@" + h.config.ImageDigest
			}
			h.mu.Lock()
			err = h.start(e)
			h.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), "retained project inputs changed") {
				t.Fatalf("changed default input was not refused at admission: %v", err)
			}
			after, _ := json.Marshal(e)
			diskAfter, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !bytes.Equal(diskBefore, diskAfter) {
				t.Fatal("refused start rewrote incarnation/spec/phase/process or durable environment record")
			}
			if _, err = os.Stat(filepath.Join(e.Spec.Directory, "workspace.ext4")); !os.IsNotExist(err) {
				t.Fatal("refused start allocated a guest disk", err)
			}
		})
	}
}
