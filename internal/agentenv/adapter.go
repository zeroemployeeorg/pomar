package agentenv

// AdapterInfo is what an actor reports about its adapter: the session
// report's adapter fields. These are reported facts, not execution authority.
type AdapterInfo struct {
	Provider string
	Name     string
	Version  string
	// Capabilities is the session report's "capabilities" object.
	Capabilities map[string]any
	// Deprecated: an actor's qualification claim is ignored. Qualification
	// comes from the owner's pinned selection and Pomar's capability policy.
	ControllerQualified bool
	// NativeMethod names the adapter's native controller request method.
	NativeMethod string
}

// Describer is implemented by an actor that reports its own adapter. An
// actor that does not is unknown until the owner selects it and its protocol
// negotiation confirms the selected identity.
type Describer interface {
	Adapter() AdapterInfo
}

// codexAdapter is the description the broker has always reported.
func codexAdapter() AdapterInfo {
	return AdapterInfo{
		Provider: "openai", Name: "codex-app-server", Version: "0.160.0",
		Capabilities: map[string]any{"version": "pomar.codex-capabilities/v1", "supported_operations": []string{"session.inspect", "login.device_code", "task.submit", "operation.inspect", "events.read", "permission.respond", "turn.interrupt", "result.export", "thread.resume_after_fence"}, "permission_response_kinds": []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"}, "permission_decisions": []string{"accept", "decline", "cancel"}},
		NativeMethod: "item/tool/call",
	}
}

// adapter reports the actor's description without treating it as a grant.
func (b *Broker) adapter() AdapterInfo {
	if d, ok := b.Actor.(Describer); ok {
		return d.Adapter()
	}
	selection := b.Store.Snapshot().AdapterSelection
	if selection != nil && selection.Provider == "openai" && b.negotiatedCodex {
		if selection.Version == "0.160.0" {
			return codexAdapter()
		}
		return AdapterInfo{Provider: selection.Provider, Name: selection.Name, Version: selection.Version, Capabilities: map[string]any{}}
	}
	return AdapterInfo{Provider: "unknown", Name: "unknown", Version: "unknown", Capabilities: map[string]any{}}
}

func (a AdapterInfo) report() map[string]any {
	return map[string]any{"provider": a.Provider, "name": a.Name, "version": a.Version, "capabilities": a.Capabilities}
}
