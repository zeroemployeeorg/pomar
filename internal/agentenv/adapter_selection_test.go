package agentenv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func qualifiedFixtureSelection() AdapterSelection {
	return AdapterSelection{Provider: "openai", Name: "codex-app-server", Version: "0.160.0", Platform: "linux/arm64", Executable: "/opt/provider/codex", ExecutableSHA256: qualifiedCodexLinux, ConfigurationSHA256: strings.Repeat("c", 64)}
}

func TestAdapterClaimsCannotGrantQualification(t *testing.T) {
	for _, change := range []string{"none", "hash", "platform", "version", "claude"} {
		t.Run(change, func(t *testing.T) {
			b := NewBroker(newStore(t), t.TempDir())
			s := qualifiedFixtureSelection()
			switch change {
			case "hash":
				s.ExecutableSHA256 = strings.Repeat("d", 64)
			case "platform":
				s.Platform = "linux/amd64"
			case "version":
				s.Version = "0.170.0"
			case "claude":
				s.Provider = "anthropic"
				s.Name = "claude-code"
				s.Version = "2.1.280"
			}
			if change != "none" {
				if err := b.ConfigureAdapterSelection(s); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.ConfigureController([]string{"echo"}); err != nil {
				t.Fatal(err)
			}
			claim := AdapterInfo{Provider: s.Provider, Name: s.Name, Version: s.Version, ControllerQualified: true}
			if err := b.Initialize(context.Background(), describedActor{info: claim}); err == nil || b.controllerReady {
				t.Fatal("unqualified/missing owner binding admitted controller")
			}
		})
	}
}

func TestOwnerSelectedAdapterUsesProviderSpecificNegotiation(t *testing.T) {
	unknown := NewBroker(newStore(t), t.TempDir())
	if err := unknown.Initialize(context.Background(), fakeActor{}); err != nil {
		t.Fatal(err)
	}
	if unknown.adapter().Provider != "unknown" {
		t.Fatal("an undescribed actor was attributed to Codex")
	}
	b := NewBroker(newStore(t), t.TempDir())
	s := qualifiedFixtureSelection()
	if err := b.ConfigureAdapterSelection(s); err != nil {
		t.Fatal(err)
	}
	if err := b.ConfigureController([]string{"echo"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(context.Background(), &controllerActor{fixtureActor: actorFixture(), version: "0.160.0"}); err != nil || !b.controllerReady || !b.negotiatedCodex {
		t.Fatalf("pinned negotiated Codex unavailable: %v", err)
	}
	if b.adapter().Version != "0.160.0" {
		t.Fatal("negotiated Codex report unavailable")
	}
	wrong := NewBroker(newStore(t), t.TempDir())
	if err := wrong.ConfigureAdapterSelection(s); err != nil {
		t.Fatal(err)
	}
	if err := wrong.ConfigureController([]string{"echo"}); err != nil {
		t.Fatal(err)
	}
	if err := wrong.Initialize(context.Background(), fakeActor{}); err == nil {
		t.Fatal("selected version was not checked against native negotiation")
	}
	mismatch := NewBroker(newStore(t), t.TempDir())
	if err := mismatch.ConfigureAdapterSelection(s); err != nil {
		t.Fatal(err)
	}
	if err := mismatch.Initialize(context.Background(), describedActor{info: AdapterInfo{Provider: "anthropic", Name: "claude-code", Version: "2.1.280", ControllerQualified: true}}); err == nil {
		t.Fatal("reported provider contradicted the owner selection")
	}
	claude := NewBroker(newStore(t), t.TempDir())
	s.Provider = "anthropic"
	s.Name = "claude-code"
	s.Version = "2.1.280"
	if err := claude.ConfigureAdapterSelection(s); err != nil {
		t.Fatal(err)
	}
	if err := claude.Initialize(context.Background(), describedActor{info: AdapterInfo{Provider: s.Provider, Name: s.Name, Version: s.Version, ControllerQualified: true}}); err != nil {
		t.Fatal("Codex negotiation was imposed on Claude", err)
	}
	if claude.controllerReady || claude.negotiatedCodex || claude.adapter().Provider != "anthropic" {
		t.Fatal("reported Claude capability became a qualified grant")
	}
}

func TestRetainedAdapterSelectionCannotChangeOnResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	s, err := Open(dir, "env", "session", "inc-one")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBroker(s, t.TempDir())
	selected := qualifiedFixtureSelection()
	if err = b.ConfigureAdapterSelection(selected); err != nil {
		t.Fatal(err)
	}
	if err = s.Thread("inc-one", "thread-one"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "env", "session", "inc-two")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current := NewBroker(s, t.TempDir())
	if err = current.ConfigureAdapterSelection(selected); err != nil {
		t.Fatal("compatible selection refused", err)
	}
	changes := []AdapterSelection{selected, selected, selected, selected, selected}
	changes[0].ConfigurationSHA256 = strings.Repeat("a", 64)
	changes[1].ExecutableSHA256 = strings.Repeat("b", 64)
	changes[2].Executable = "/different/codex"
	changes[3].Platform = "darwin/arm64"
	changes[4].Version = "0.159.0"
	for _, changed := range changes {
		if err = current.ConfigureAdapterSelection(changed); err == nil {
			t.Fatal("retained execution configuration changed")
		}
		if *s.Snapshot().AdapterSelection != selected {
			t.Fatal("refusal altered the retained binding")
		}
	}
	legacy := newStore(t)
	if err = legacy.Thread("actor-one", "old-thread"); err != nil {
		t.Fatal(err)
	}
	if err = NewBroker(legacy, t.TempDir()).ConfigureAdapterSelection(selected); err == nil {
		t.Fatal("legacy thread was retrospectively attributed")
	}
}

func TestSelectionRecordsDigestsWithoutLaunchConfigurationBytes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(binary, []byte("synthetic-provider-binary"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := SelectAdapterBinary("openai", "codex-app-server", "0.160.0", binary, []string{"app-server", "synthetic-config-canary"}, []string{"TEST_CONFIGURATION=synthetic-env-canary"})
	if err != nil {
		t.Fatal(err)
	}
	if !adapterDigest.MatchString(s.ExecutableSHA256) || !adapterDigest.MatchString(s.ConfigurationSHA256) {
		t.Fatal("missing binding digest")
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "synthetic-config-canary") || strings.Contains(string(raw), "synthetic-env-canary") {
		t.Fatal("launch configuration bytes entered the journal")
	}
	changed, err := SelectAdapterBinary("openai", "codex-app-server", "0.160.0", binary, []string{"changed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ConfigurationSHA256 == s.ConfigurationSHA256 || changed.ExecutableSHA256 != s.ExecutableSHA256 {
		t.Fatal("configuration/executable identity conflated")
	}
}
