package agentenv

// AdapterInfo is what an actor reports about its adapter: the session
// report's adapter fields, and whether the controller bridge is qualified
// for it (POMAR-CC proposal P2). The shared broker no longer assumes the
// actor is Codex.
type AdapterInfo struct {
	Provider string
	Name     string
	Version  string
	// Capabilities is the session report's "capabilities" object.
	Capabilities map[string]any
	// ControllerQualified is true only when the controller bridge's native
	// tool exchange is qualified for this adapter and version.
	ControllerQualified bool
	// NativeMethod names the adapter's native controller request method.
	NativeMethod string
}

// Describer is implemented by an actor that reports its own adapter. An
// actor that does not is described as Codex app-server 0.160.0, as before.
type Describer interface {
	Adapter() AdapterInfo
}

// codexAdapter is the description the broker has always reported.
func codexAdapter() AdapterInfo {
	return AdapterInfo{
		Provider: "openai", Name: "codex-app-server", Version: "0.160.0",
		Capabilities:        map[string]any{"version": "pomar.codex-capabilities/v1", "supported_operations": []string{"session.inspect", "login.device_code", "task.submit", "operation.inspect", "events.read", "permission.respond", "turn.interrupt", "result.export", "thread.resume_after_fence"}, "permission_response_kinds": []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"}, "permission_decisions": []string{"accept", "decline", "cancel"}},
		ControllerQualified: true, NativeMethod: "item/tool/call",
	}
}

// adapter is the actor's own description, or Codex's for an actor without one.
func (b *Broker) adapter() AdapterInfo {
	if d, ok := b.Actor.(Describer); ok {
		return d.Adapter()
	}
	return codexAdapter()
}

func (a AdapterInfo) report() map[string]any {
	return map[string]any{"provider": a.Provider, "name": a.Name, "version": a.Version, "capabilities": a.Capabilities}
}
