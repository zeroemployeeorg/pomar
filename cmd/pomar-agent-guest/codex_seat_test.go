package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexSeatUsesGuestOnlyEnvironmentAndItsOwnSupervisor(t *testing.T) {
	env := strings.Join(codexSeatEnv("example-seat"), "\n")
	for _, bad := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "CLAUDE_CONFIG_DIR"} {
		if strings.Contains(env, bad) {
			t.Fatal("host or other provider config inherited")
		}
	}
	if !strings.Contains(env, "CODEX_HOME=/pomar/job/.codex") {
		t.Fatal("guest home missing")
	}
	if !slices.Contains(codexSeatEnv("example-seat"), "ZMAIL_URL=http://127.0.0.1:7074") || !slices.Contains(codexSeatEnv("example-seat"), "ZMAIL_INSTANCE=example-seat") || !slices.Contains(codexSeatEnv("example-seat"), "NO_PROXY=127.0.0.1,localhost") || !slices.Contains(codexSeatEnv("example-seat"), "no_proxy=127.0.0.1,localhost") {
		t.Fatal("mail client does not use the fixed local channel and seat")
	}
	args := codexTmuxEnsure("example", "/pomar/guest")
	i := slices.Index(args, "--")
	if i < 0 || args[i+1] != "/pomar/guest" || args[i+2] != "codex-seat-console" || slices.Contains(args[:i], "/opt/codex") {
		t.Fatal("tmux starts actor directly", args)
	}
}

func TestGuestLaunchRequiresCompleteExactPolicy(t *testing.T) {
	good := `{"sandbox":"workspace-write","approvals":"never","network_access":true,"model":"fixture-model","effort":"high","cwd":"/work"}`
	if _, err := parseCodexLaunch(good); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", `{}`, good + `{}`, strings.Replace(good, `"cwd":"/work"`, `"cwd":"/other"`, 1), strings.Replace(good, `"cwd":"/work"`, `"cwd":"/work","arbitrary_command":"shell"`, 1)} {
		if _, err := parseCodexLaunch(raw); err == nil {
			t.Fatal("invalid guest launch accepted")
		}
	}
}

func TestCodexCustodianTerminalDoesNotLoadActorConfigOrCommands(t *testing.T) {
	args := codexTmuxEnsure("example", "/custodian/guest")
	if slices.Index(args, codexTmuxConfig) < 0 || slices.Index(args, codexTmuxSocket) < 0 {
		t.Fatal("custodian terminal missing private config/socket", args)
	}
	for _, required := range []string{"prefix None", "status off", "allow-rename off", "set-titles off", "set-clipboard off", "allow-passthrough off", "prefix2 None", "default-shell /bin/false", `update-environment ""`, "unbind-key -a -T root", "unbind-key -a -T copy-mode", "unbind-key -a -T copy-mode-vi"} {
		if !strings.Contains(codexCustodianTmuxConfig, required) {
			t.Fatal("custodian command path left available", required)
		}
	}
	if strings.Contains(strings.Join(codexCustodianEnv(), "\n"), seatHome) {
		t.Fatal("custodian terminal uses actor home")
	}
}
