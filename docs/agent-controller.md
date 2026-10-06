# Bounded native controller requests

`pomar.controller/v1` adds an opt-in request/reply transport to a development
agent environment. It does not run host commands or implement message routing,
recipient identities, inbox ordering, acknowledgements or organisational policy.
The controller owns those decisions and authenticates requests through the
existing owner-private socket. Guest network access and CI are unchanged.

## Adapter and owner configuration

The supported native adapter is **Codex app-server 0.160.0**. The protocol was
checked against that installed binary's `app-server generate-json-schema
--experimental` output: `InitializeParams`, `ThreadStartParams`,
`DynamicToolCallParams` and `DynamicToolCallResponse`. This deliberately pinned
experimental integration requires requalification before supporting another
adapter/version. It never converts user-input or approval responses into data.
See the [native protocol documentation](https://learn.chatgpt.com/docs/app-server).

An owner sets `controllerCapabilities` in the development host configuration
or a named environment profile, for example `["inbox", "ack", "answer"]`.
Profiles use their own list, not another profile's capabilities. At most 16
distinct names match `^[a-z][a-z0-9_]{0,63}$`. No capability is enabled by default.
The Swift owner passes those names to the root guest broker's
`-controller-capabilities` flag. Configuration is not an actor/controller HTTP
operation; names cannot contain commands, socket paths or recipient addresses.
The tool set cannot be added to or changed on an existing native thread.

Initialization sends `capabilities: {"experimentalApi": true}` only when tools
are configured. The adapter-reported `userAgent` must identify version 0.160.0;
this is a compatibility check, not binary attestation. `thread/start` registers
the following function in `dynamicTools` (the enum is the owner's actual list):

```json
{
  "type": "function",
  "name": "pomar_controller_request",
  "description": "Request a configured controller capability with bounded data. Delivery is not an acknowledgement. Never include credentials, host commands, socket paths or replacement recipient identities.",
  "inputSchema": {
    "type": "object",
    "additionalProperties": false,
    "required": ["capability", "data"],
    "properties": {
      "capability": {"type": "string", "enum": ["inbox", "ack", "answer"]},
      "data": {"type": "string", "maxLength": 32768}
    }
  }
}
```

`thread/resume` restores the native thread's previously registered tools;
Pomar preserves and checks the configuration. Pending requests from a fenced
incarnation cannot be answered. Reconnecting a controller to a surviving broker
preserves pending requests; replacing a broker does not authorize replay.

## Native request and controller reply

The native server sends `item/tool/call` with a string/integer JSON-RPC ID and
these exact parameters (optional `namespace` must be absent, null or empty):

```json
{
  "threadId": "native-thread",
  "turnId": "native-turn",
  "callId": "native-call",
  "tool": "pomar_controller_request",
  "arguments": {"capability": "inbox", "data": "bounded application data"}
}
```

Arguments and replies carry text only, bounded to **32768 UTF-8 bytes**.
Objects, extra fields, unconfigured capabilities, oversized data, unsupported
tools/namespaces and unknown, completed or stale turns are refused. Text is
application data, never an executable command, a socket to open, credentials
to install, or a replacement for an independently retained recipient binding.
Controllers must map each configured capability to their fixed approved action
and validate its application data; the broker performs no host action.

`GET /v1/controller/requests` returns `contract`, `native_method`,
`adapter_version`, `supported`, `configured_capabilities`, and `requests` keyed
by request ID. `supported` means the configured, version-checked adapter
negotiation succeeded; actual native calls also require a registered thread
and active bound turn. Each request contains:

```json
{
  "request_id": "sha256-of-canonical-binding",
  "binding": {
    "environment_id": "environment",
    "session_id": "session",
    "incarnation": "actor-one",
    "operation_id": "operation",
    "input_sha256": "retained-task-input-digest",
    "thread_id": "native-thread",
    "turn_id": "native-turn",
    "native_request_id": 42,
    "call_id": "native-call"
  },
  "capability": "inbox",
  "data": "bounded application data",
  "native_params_sha256": "sha256-of-original-native-parameter-bytes",
  "state": "pending"
}
```

Post to `/v1/controller/requests/{request_id}/reply` with **all four fields**:
`reply_id` (a unique operation ID), `binding` (the complete binding above),
`text` (string), and `success` (boolean). Unknown fields/trailing JSON are
refused. The native answer's exact schema is:

```json
{"contentItems": [{"type": "inputText", "text": "controller response"}], "success": true}
```

The broker syncs the reply ID, full binding, text and SHA-256 of this canonical
native response JSON before writing to the native request ID. Concurrent or
identical retries return the retained outcome without another write. A changed
reply ID, text or success conflicts; a changed binding is fenced. Reply IDs
cannot be reused across requests. HTTP uses 200 for retained outcomes,
400 for malformed bodies, 403 for stale bindings and 409 for conflicts/holds.
Request bodies are limited to 64 KiB; excessively escaped text may hit this
transport limit before the decoded-text limit.

States are `pending`, `dispatching`, `written`, `acceptance_unknown`, or
`fenced`. `written` means a successful local transport write, not proof of
native consumption. A failed/short write or an interrupted dispatch remains
unresolved and is never automatically replayed, including after restart.
Matching native `item/completed` evidence and the requesting actor's answer
can qualify consumption; this API does not currently reconcile an uncertain
write. Native tool completion and delivery remain separate from recipient
acknowledgement. An acknowledgement or answer requires its own new
authenticated actor call through a configured capability.

The existing host proxy exposes these routes at
`/v1/environments/{id}/agent/controller/requests` and the corresponding
`/{request_id}/reply` path. There is no remote owner-socket exposure or new
credential route. Existing capability/permission routes remain unchanged.

## Qualification

The regular test suite checks bindings, stale/completed turns, configured
capabilities, duplicates, changed replies, uncertain writes, concurrent replies,
durability-before-delivery, persistence failure and restart fencing.

An explicit native qualification uses an already installed, already signed-in
adapter. It creates an isolated **read-only ephemeral thread**, requests a
bounded echo capability, replies with a fresh nonce through the HTTP route,
and requires matching native tool completion, actor nonce echo and completed
turn. It reads/copies no credential stores. Run with:

```sh
POMAR_NATIVE_CODEX="$HOME/path/to/installed/codex" \
  go test -count=1 -v -run '^TestControllerRealNativeExchange$' ./internal/agentenv
```

The result belongs to the actual platform executed. A native macOS exchange
does not qualify Linux VM delivery or an external recipient gateway. Those
remain separate owner-authorized checks using the reviewed environment,
fixed controller actions and independently retained recipient/public-key
bindings. This source change does not mutate a retained live guest or admit
a runtime actor.
