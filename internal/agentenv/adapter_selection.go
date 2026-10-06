package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
)

// AdapterSelection is supplied by the trusted owner, never by the actor.
// ConfigurationSHA256 binds launch arguments and the explicit environment,
// excluding authentication material. Mutable provider settings need their own
// effective-configuration binding before they can be claimed compatible.
type AdapterSelection struct {
	Provider            string `json:"provider"`
	Name                string `json:"name"`
	Version             string `json:"version"`
	Platform            string `json:"platform"`
	Executable          string `json:"executable"`
	ExecutableSHA256    string `json:"executable_sha256"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}

var adapterDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SelectAdapterBinary hashes the selected public executable, not a credential
// store. Only launch configuration digests enter the retained journal.
func SelectAdapterBinary(provider, name, version, binary string, args, env []string) (AdapterSelection, error) {
	f, err := os.OpenFile(binary, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return AdapterSelection{}, errors.New("selected adapter executable unavailable")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 1<<30 {
		return AdapterSelection{}, errors.New("selected adapter is not a bounded regular executable")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return AdapterSelection{}, errors.New("selected adapter executable unreadable")
	}
	config, err := json.Marshal(struct{ Args, Env []string }{args, env})
	if err != nil {
		return AdapterSelection{}, err
	}
	sum := sha256.Sum256(config)
	return AdapterSelection{Provider: provider, Name: name, Version: version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Executable: binary, ExecutableSHA256: hex.EncodeToString(h.Sum(nil)), ConfigurationSHA256: hex.EncodeToString(sum[:])}, nil
}

func (b *Broker) ConfigureAdapterSelection(selection AdapterSelection) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Actor != nil {
		return errors.New("adapter selection requires an uninitialized broker")
	}
	if !filepath.IsAbs(selection.Executable) || len(selection.Executable) > 4096 || !adapterDigest.MatchString(selection.ExecutableSHA256) || !adapterDigest.MatchString(selection.ConfigurationSHA256) || selection.Version == "" || len(selection.Version) > 64 || selection.Platform == "" || len(selection.Platform) > 32 {
		return errors.New("adapter selection requires a pinned executable and configuration")
	}
	if !((selection.Provider == "openai" && selection.Name == "codex-app-server") || (selection.Provider == "anthropic" && selection.Name == "claude-code")) {
		return errors.New("unsupported owner adapter selection")
	}
	m := b.Store
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(b.incarnation); err != nil {
		return err
	}
	if old := m.s.AdapterSelection; old != nil {
		if *old != selection {
			return fmt.Errorf("retained adapter selection changed: %w", ErrConflict)
		}
		return nil
	}
	if m.s.ThreadID != "" {
		return errors.New("retained thread has no adapter binding; continue its previous binary or use a distinct environment")
	}
	m.s.AdapterSelection = &selection
	return m.save()
}

// These executable pins have native echo qualification. A provider
// descriptor or version string cannot add a record to this policy. Each new
// version/platform needs its own bounded native qualification and review.
const qualifiedCodexLinux = "50b06603bdcdac39b714f5c3e68583c002b8ad8779ebfdaaf4932ff016b379c0"
const qualifiedCodexDarwin = "112fae7a5a1223e673c8a1791d32338f37df8b527ff1159bb8adac6c4dbf1b4b"

// Claude Code 2.1.280's controller request tool (internal/claudeactor),
// qualified natively against this darwin/arm64 executable by
// TestLiveControllerTool. Its linux/arm64 executable is pinned only after its
// own in-guest qualification.
const qualifiedClaudeDarwin = "387a5c5dcdbb815085edf0baf79591f9d8894efe922bceaf3d75b1b08055229d"

func qualifiedController(selection *AdapterSelection) bool {
	if selection == nil {
		return false
	}
	if selection.Provider == "anthropic" && selection.Name == "claude-code" && selection.Version == "2.1.280" {
		return selection.Platform == "darwin/arm64" && selection.ExecutableSHA256 == qualifiedClaudeDarwin
	}
	if selection.Provider != "openai" || selection.Name != "codex-app-server" || selection.Version != "0.160.0" {
		return false
	}
	return (selection.Platform == "linux/arm64" && selection.ExecutableSHA256 == qualifiedCodexLinux) || (selection.Platform == "darwin/arm64" && selection.ExecutableSHA256 == qualifiedCodexDarwin)
}
