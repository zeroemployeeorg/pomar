package main

// Owner operations are discoverable without attempting privileged execution.
// Naming them is not authorization to provision, admit or alter a service.
func ownerCatalogue() []command {
	return []command{
		{"install", "Install a verified release in your user prefix", "pomar install -bundle DIR [-prefix DIR]", "text", []string{"no sudo or service activation"}},
		{"upgrade stage", "Verify and retain a controlled upgrade payload", "pomar upgrade stage -bundle DIR -manifest-sha256 SHA256", "JSON", []string{"administrator only; frozen signed CLI and owner-approved manifest required"}},
		{"upgrade check", "Check the installed baseline and controlled payload", "pomar upgrade check -bundle ROOT_STAGE -manifest-sha256 SHA256 [-archive ORIGINAL_ARCHIVE]", "JSON", []string{"administrator only; readbacks do not perform apply"}},
		{"upgrade apply", "Apply a checked existing-service transaction", "pomar upgrade apply -bundle ROOT_STAGE -manifest-sha256 SHA256", "JSON", []string{"administrator only; requires matched idle owner and retained rollback"}},
		{"upgrade rollback", "Restore the archived existing-service build", "pomar upgrade rollback -bundle ROOT_STAGE -manifest-sha256 SHA256 -archive ORIGINAL_ARCHIVE", "JSON", []string{"administrator only; preserve the transaction journal and archive"}},
		{"server prepare", "Prepare the release-pinned development base", "pomar server prepare -root ABSOLUTE_DIR [-image NAME]", "text", []string{"development owner only; new dedicated root"}},
		{"server kernel", "Acquire the pinned development kernel", "pomar server kernel -root ABSOLUTE_DIR", "text", nil},
		{"server start", "Run a foreground development manager", "pomar server start -root ABSOLUTE_DIR -kernel-sha256 HEX [MANAGER_OPTIONS]", "text", []string{"no launch daemon or account provisioning"}},
		{"seat profile", "Compile a declared seat profile", "pomar seat profile -root DIR -classes FILE -source-bundle FILE SEAT", "JSON", nil},
		{"seat up", "Create and start an owner-approved declared environment", "pomar seat up -root DIR -records REC -classes FILE [-here WHERE] SEAT", "JSON receipt or text when already running", nil},
		{"seat attach", "Attach to an environment terminal", "pomar seat attach -root DIR -environment ENV", "interactive terminal", nil},
		{"seat stop", "Stop the declared environment", "pomar seat stop -root DIR -records REC SEAT", "JSON receipt or text when already stopped", nil},
		{"venue status", "Read scoped storage and ledger inconsistencies", "pomar venue status -root DIR", "text", []string{"exit 3 for unaccounted entries"}},
		{"venue init", "Create a venue structure", "pomar venue init -root DIR", "text", nil},
		{"venue classify", "Classify a previously ledgered object", "pomar venue classify -root DIR -kind K -id ID -class attempt|cache", "text", nil},
		{"attempt reconcile", "Read manager reconciliation findings", "pomar attempt reconcile -root DIR", "JSON", []string{"host-wide authorization required"}},
		{"attempt vm-orphans", "Read unaccounted VM services", "pomar attempt vm-orphans -root DIR", "JSON", []string{"reports only; never signals them"}},
		{"attempt drain", "Set or lift the owner's admission fence", "pomar attempt drain -root DIR [-off]", "JSON", []string{"existing work may finish; not a caller-authorized recovery act"}},
		{"attempt caches", "Read controlled cache retention plan", "pomar attempt caches -root DIR", "JSON", nil},
		{"attempt evict", "Apply controlled cache retention", "pomar attempt evict -root DIR", "JSON", nil},
		{"attempt rm", "Remove a terminal attempt record", "pomar attempt rm -root DIR -id ID", "JSON", []string{"preserve unique receipts before removal"}},
		{"image check", "Verify a pinned OCI layout", "pomar image check -layout DIR -digest D [-files MANIFEST]", "JSON", nil},
		{"image load", "Load a verified pinned image", "pomar image load -root DIR -host-bin PATH -layout DIR -digest D -name REPO:TAG", "JSON", nil},
		{"base build", "Build a pinned guest base", "pomar base build -root DIR -host-bin PATH [-image NAME]", "text", nil},
		{"mirror sync", "Sync a configured source mirror", "pomar mirror sync -root DIR -name NAME -url URL", "text", nil},
		{"mirror resolve", "Resolve a configured mirror ref", "pomar mirror resolve -root DIR -name NAME -ref REF", "text", nil},
		{"volume create", "Create a bounded test volume", "pomar volume create -root DIR -id ID -size-mib N", "text", nil},
		{"volume rm", "Remove a test volume", "pomar volume rm -root DIR -id ID", "text", nil},
		{"smoke fetch-kernel", "Fetch the catalogue-pinned kernel", "pomar smoke fetch-kernel -root DIR -host-bin PATH", "text", nil},
		{"smoke boot", "Run a pinned boot probe", "pomar smoke boot -root DIR -host-bin PATH -kernel-sha256 HEX -id ID", "text", nil},
		{"smoke boundaries", "Admit and grade the read-only source boundary probe", "pomar smoke boundaries -socket PATH -mirror NAME [-ref REF] [-class NAME] [-id ID]", "JSON", []string{"starts real work; requires ordinary job authorization"}},
		{"manager", "Low-level foreground manager configuration", "pomar manager -root DIR -host-bin PATH -kernel-sha256 HEX [OPTIONS]", "text", []string{"see pomar help all; source/development owners only"}},
	}
}

type catalogueFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Meaning string `json:"meaning"`
}

func catalogueFlags() map[string][]catalogueFlag {
	return map[string][]catalogueFlag{
		"actor-switch":      {{"root", "path", "Owner host data root"}, {"environment", "string", "Retained environment ID"}, {"session", "string", "Stable seat session ID"}, {"incarnation", "string", "Original actor incarnation"}, {"id", "string", "Durable original replacement operation ID"}, {"profile", "string", "Owner-configured target harness profile"}, {"status", "bool", "Inspect original operation without dispatch"}},
		"manager_selection": {{"socket", "path", "Explicit approved manager socket; POMAR_SOCKET is the environment fallback"}, {"root", "path", "Explicit local owner's root; not another seat's data"}},
		"doctor":            {{"json", "bool", "Emit the diagnostic array instead of text"}},
		"health":            {{"json", "bool", "Emit pomar.health/v1 instead of text"}, {"expected-key-id", "hex", "Independent SHA256 of manager result public key"}, {"timeout", "duration", "Total read deadline; default8s, range100ms..30s"}},
		"run":               {{"id", "string", "Required unique original attempt identity"}, {"class", "string", "Configured class authorized to this caller"}, {"wait", "bool", "Wait after admission"}, {"timeout", "duration", "Client observation deadline; never stops the job"}, {"mirror", "string", "Owner-configured source mirror"}, {"ref", "string", "Reachability reference"}, {"sha", "hex", "Exact40-digit source commit"}, {"input", "repeatable string", "NAME=PATH immutable input"}, {"output", "repeatable string", "Named copied-out output"}},
		"wait":              {{"id", "string", "Original attempt to observe"}, {"timeout", "duration", "Observation deadline; exit75 leaves execution alone"}},
		"attempt_reads":     {{"id", "string", "Original authorized attempt identity"}},
		"logs":              {{"id", "string", "Terminal attempt"}, {"public-key", "base64", "Independent result public key"}, {"o", "path", "Output destination instead of stdout"}},
		"attempt output":    {{"id", "string", "Terminal attempt"}, {"name", "string", "Named output bound in the result"}, {"o", "path", "Required destination"}},
		"result verify":     {{"reply", "path", "Original signed reply file"}, {"public-key", "base64", "Independently retained Ed25519 public key"}},
	}
}
