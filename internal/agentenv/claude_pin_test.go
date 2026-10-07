package agentenv

import (
	"strings"
	"testing"
)

// Claude Code's controller is qualified only for its pinned, natively
// qualified executable; another platform, executable or version is not.
func TestClaudeControllerPin(t *testing.T) {
	pinned := AdapterSelection{Provider: "anthropic", Name: "claude-code", Version: "2.1.280", Platform: "darwin/arm64", Executable: "/opt/pomar-claude/bin/claude", ExecutableSHA256: qualifiedClaudeDarwin, ConfigurationSHA256: strings.Repeat("c", 64)}
	if !qualifiedController(&pinned) {
		t.Fatal("the qualified Claude Code executable is not qualified")
	}
	linux := pinned
	linux.Platform, linux.ExecutableSHA256 = "linux/arm64", qualifiedClaudeLinux
	if !qualifiedController(&linux) {
		t.Fatal("the qualified linux/arm64 executable is not qualified")
	}
	for name, mutate := range map[string]func(*AdapterSelection){
		"the darwin executable on linux":    func(s *AdapterSelection) { s.Platform = "linux/arm64" },
		"the linux executable on darwin":    func(s *AdapterSelection) { s.ExecutableSHA256 = qualifiedClaudeLinux },
		"another executable":                func(s *AdapterSelection) { s.ExecutableSHA256 = strings.Repeat("e", 64) },
		"another version":                   func(s *AdapterSelection) { s.Version = "2.1.285" },
		"the Codex pin under Claude's name": func(s *AdapterSelection) { s.Platform, s.ExecutableSHA256 = "linux/arm64", qualifiedCodexLinux },
		"Claude's pin under Codex's name":   func(s *AdapterSelection) { s.Provider, s.Name, s.Version = "openai", "codex-app-server", "0.160.0" },
	} {
		s := pinned
		mutate(&s)
		if qualifiedController(&s) {
			t.Errorf("%s: qualified", name)
		}
	}
}
