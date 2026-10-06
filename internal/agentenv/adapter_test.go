package agentenv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The Codex report is byte for byte what the broker reported before actors
// could describe themselves (golden: the literal it replaced).
func TestCodexAdapterReportIsUnchanged(t *testing.T) {
	golden := map[string]any{"provider": "openai", "name": "codex-app-server", "version": "0.160.0", "capabilities": map[string]any{"version": "pomar.codex-capabilities/v1", "supported_operations": []string{"session.inspect", "login.device_code", "task.submit", "operation.inspect", "events.read", "permission.respond", "turn.interrupt", "result.export", "thread.resume_after_fence"}, "permission_response_kinds": []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"}, "permission_decisions": []string{"accept", "decline", "cancel"}}}
	want, _ := json.Marshal(golden)
	got, _ := json.Marshal(codexAdapter().report())
	if string(got) != string(want) {
		t.Fatalf("Codex adapter report changed:\n got %s\nwant %s", got, want)
	}
}

type describedActor struct {
	fakeActor
	info AdapterInfo
}

func (d describedActor) Adapter() AdapterInfo { return d.info }

type fakeActor struct{}

func (fakeActor) Call(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{"userAgent":"other/1.0 "}`), nil
}
func (fakeActor) Notify(string, any) error          { return nil }
func (fakeActor) Answer(json.RawMessage, any) error { return nil }
func (fakeActor) Done() <-chan struct{}             { return make(chan struct{}) }

// A describing actor reports itself. Its declaration cannot grant controller
// access, and a provider-specific negotiation requires an owner selection.
func TestDescribingActorGatesTheControllerBridge(t *testing.T) {
	open := func(t *testing.T, caps []string) *Broker {
		s, err := Open(t.TempDir()+"/state", "env-1", "session-1", "inc-1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		b := NewBroker(s, "/work")
		if err := b.ConfigureController(caps); err != nil {
			t.Fatal(err)
		}
		return b
	}
	info := AdapterInfo{Provider: "anthropic", Name: "claude-code", Version: "2.1.280", Capabilities: map[string]any{"version": "pomar.claude-capabilities/v1"}}
	b := open(t, nil)
	if err := b.Initialize(context.Background(), describedActor{info: info}); err != nil {
		t.Fatal(err)
	}
	if r := b.adapter().report(); r["provider"] != "anthropic" || r["name"] != "claude-code" || r["version"] != "2.1.280" {
		t.Fatalf("report %v", r)
	}
	if err := open(t, []string{"inbox"}).Initialize(context.Background(), describedActor{info: info}); err == nil || !strings.Contains(err.Error(), "not qualified") {
		t.Fatalf("an unqualified adapter with controller capabilities: %v", err)
	}
	info.ControllerQualified = true
	if err := open(t, []string{"inbox"}).Initialize(context.Background(), describedActor{info: info}); err == nil {
		t.Fatal("an adapter's own claim granted controller authority")
	}
	// An actor that does not describe itself keeps the Codex userAgent gate.
	if err := open(t, []string{"inbox"}).Initialize(context.Background(), fakeActor{}); err == nil {
		t.Fatal("a non-Codex userAgent passed the Codex gate")
	}
}
