# Agent environments

Experimental development interface, `pomar.agent/v1`. Agent environments are separate from the
ephemeral CI attempt API. No CI daemon configuration changes are needed.

An environment has a private, retained Linux root filesystem and workspace,
resource limits, and a network policy. A session has a stable ID and a Codex
thread. Each replacement of its running actor has a new incarnation. Disconnecting
a client changes none of these identities. Stopping a VM retains its filesystem.

## Boundaries

The shared development service owns the VM helpers and private state. Each
environment has its own VM, root filesystem, credentials, network lease and
guest broker. No host home directory is mounted. The coding process runs as
uid 1000; the guest broker and its journal run as root. The agent cannot read
the broker socket or authorize its own requests. This protects the broker
from the guest agent, not from a hostile host process with the service owner's UID.

There is no guest network interface. HTTPS goes through a loopback-to-vsock
relay and a host CONNECT proxy allowing only configured exact DNS names on
port 443. Connections to private, loopback, link-local or unspecified addresses
are refused after DNS resolution. Revocation closes established tunnels too.
No credentials are held by the network proxy. Device-code sign-in occurs inside
the guest; host credential caches are never copied into it.

## Controller contract

All writes except creation carry `expected_incarnation` and a caller-supplied `operation_id`.
Creation carries the caller's environment ID and operation ID. Environment IDs
use ASCII letters, digits, underscores and hyphens; operation IDs additionally
allow dots. Both start with a letter or digit and are at most 128 bytes.
Same ID and payload returns its retained state. Changed bytes conflict. A
submission is recorded and synced before dispatch. A crash or timeout between
dispatch and the agent's reply leaves `acceptance_unknown`; inspection never
silently repeats the task. A controller must explicitly reconcile or submit a
different operation after learning the previous outcome.

The public API has no seats, organisational roles, recipient acknowledgements
or escalation policy. Callers may bind their own opaque identifiers externally.

| Route | Meaning |
| --- | --- |
| `GET /v1/session` | Environment/session/incarnation, authentication, actor status, operations |
| `POST /v1/login` | Start supported Codex device-code login; interactive result is not journaled |
| `POST /v1/tasks` | Submit text with operation ID and expected incarnation |
| `GET /v1/operations/{id}?session_id=S&expected_incarnation=I` | Inspect the original operation without another dispatch |
| `GET /v1/events?after=N` | Durable ordered progress and permission requests |
| `POST /v1/permissions/{id}` | Current incarnation's answer to a pending execution request |
| `POST /v1/interrupt` | Interrupt the current turn without replacing the session |
| `GET /v1/result` | Retained workspace diff and recorded test/command evidence |

The host service additionally creates, starts, inspects, stops and replaces
environments. It must revoke network access before stopping an old VM, and
refuse replacement until old execution departure is confirmed. An unknown
stop stays unknown: no new actor may share the writable disk. A revoked old
incarnation cannot submit commands, answer permissions or report accepted
results through the current interface. Local execution revocation requires
confirmed VM stop; closing only the reporting path is insufficient.

The service listens on its owner-only Unix socket, mode 0600, in a private
directory. Guest routes above are reached through
`/v1/environments/{environment}/agent/`. This is local operating-system
authentication for the service owner's principal. It is not a remote network
API. A controller on another host needs a separately agreed authenticated
transport; no account, SSH, firewall or permanent-service changes are implied.

Host routes:

| Route | Meaning |
| --- | --- |
| `POST /v1/environments` | `{id, operation_id}`; allocate a retained identity |
| `GET /v1/environments/{id}` | Retained lifecycle and helper observation |
| `POST /v1/environments/{id}/start` | Start a created or confirmed-stopped environment |
| `POST /v1/environments/{id}/stop` | Revoke access and request confirmed execution stop |
| `POST /v1/environments/{id}/replace` | Stop/fence the current actor before starting a successor |
| `GET /v1/environments/{id}/operations/{operation}?session_id=S&expected_incarnation=I` | Inspect the original lifecycle action |

Lifecycle writes contain `{operation_id, expected_incarnation}`. `start`
allocates the actor incarnation; allocation is not guest readiness. Inspect the
guest session for a responding adapter. A retained host phase or PID alone
does not prove that the VM or agent is alive.

For different projects in one development service, the owner may configure
named `profiles`. Each profile pins its base/image, source bundle/commit and
exact permitted HTTPS hosts. Creation additionally accepts `profile`; callers
select a configured name, never filesystem paths or arbitrary image references.
Omitting it retains the original default behaviour. Each environment keeps its
own identity, disk, session and network lease; stopped environments count against
neither the active-environment limit nor running VM resources. Resource caps and
the host's maximum live count remain owner-controlled.

A duplicate creation cannot switch profiles. Restart adopts current supervisor
artifacts but refuses a changed source/base/image binding for both named profiles
and profile-less environments. A missing named profile also refuses restart.
Use a distinct environment for different project inputs; never silently
replace the retained workspace. No host home or another project's credential
store is copied into a new environment.

Network policy is owner-controlled across restarts, not immutable. A new
incarnation adopts the current profile's `allowedHosts`; source/base/image
checks do not reject a changed permitted-host list. Inspect the effective
`environment.spec.allowedHosts` through `GET /v1/environments/{id}` and retain
it with that spec's environment/session/incarnation/source identity before
task submission. A controller must assess that effective policy against its
task scope rather than assuming a prior incarnation's policy still applies.

Owner source bundles may include release tags. Initial setup imports those tags
along with the selected HEAD so release preparation can compare the published
tag and retained project source without network Git access.

### Inspection, retention and scope fencing

Task/control lookup requires the original session and incarnation. It may
return historical evidence for a known old operation. A missing operation is
labelled `durable_non_acceptance` only in the matching, intact current journal.
That legacy wire label means current journal absence, not proof that a delayed
earlier POST cannot still commit. ZEO-RT calls this observation
`CurrentJournalAbsence`; standalone controllers must preserve the same limitation.
Keep the original operation ID, input digest and incarnation for inspection.
Absence alone grants neither replay nor release authority; this interface does
not establish a stronger non-acceptance or general retry protocol. A missing
or inaccessible journal, identity mismatch, old incarnation lookup miss or
unavailable broker is unknown. Never replay based on an unbound 404 or 502.
Changing any authority-bearing input under a recorded ID conflicts. Login,
permission decisions and interrupts are also journaled before dispatch.

Permission IDs are opaque keys bound to the originating actor incarnation and
native request ID. A provider may reuse a native ID after replacement without
colliding with retained permission evidence. Controllers must still send the
current expected incarnation. An answer naming no live pending permission is
retained as `refused` with `durable_non_acceptance`: that answer did not reach the
actor. Its operation ID remains bound to its original input, so a later pending
request cannot turn an identical retry into a dispatch. This differs from an
answer attempted with uncertain delivery, which remains `acceptance_unknown`.

The journal retains at most 8,192 events, 256 KiB per event and 16 MiB overall.
There is no silent eviction: exceeding a bound closes the actor protocol
connection and requires execution fencing before recovery. Event sequences
continue across incarnations. `GET events` returns `session_id`, `incarnation`,
`first_sequence`, `last_sequence`, `next_cursor`, `gap` and `events`; each event
has its originating incarnation. A cursor beyond retained evidence conflicts.
There is no timer-based expiry or automatic data pruning.

A stop/replacement action retains its old scope fence:

```json
{
  "incarnation": "old-actor",
  "scope_id": "scope-old-actor",
  "network_revoked": true,
  "adapter_access_revoked": true,
  "workspace_write_revoked": false,
  "termination_confirmed": false,
  "state": "execution_unknown"
}
```

Only a matching VM-owner non-execution/stop receipt plus exact helper departure
changes the last two booleans to true and the state to `confirmed`. Pending
fencing never admits a replacement. A fresh process's absence alone is not a
VM stop receipt. Previously completed external effects remain historical.

Results are available only when every task has a completion disposition or an
explicit external-controller `closed_unresolved` continuation binding. An unresolved
predecessor never becomes a successful actor result; the reserved successor must
have its own matching terminal operation record, including before submission.
An unhealthy journal holds export. Workspace inventory remains separately available.
Export runs Git as the coding user, disables external diff/text conversion,
limits the binary diff to 4 MiB and limits untracked regular files to 32 files
and 4 MiB total. Symlinks and paths escaping the workspace are refused. This
is a bounded development export, not an immutable publication artifact.

Result export returns HTTP 422 when changed tracked or untracked paths name a
credential store or private configuration: `.codex`, `.claude`, `.ssh`, `.aws`,
`.azure`, `.kube`, `.credentials.json`, `.netrc`, `.npmrc`, `.pypirc`, common SSH
private-key names, or `.env` and its variants. `.env.example`, `.env.sample` and
`.env.template` remain ordinary source templates. Tracked diffs use only the
literal paths checked by the exporter, so a new path cannot join the diff
between the check and export. This policy does not discover or redact secrets
copied into ordinary source files, templates or actor messages. Treat result
content as private, untrusted workspace data requiring review before sharing;
an egress allowlist does not make exported content safe to publish.

For actual wheel and source-distribution bytes, use the
[bounded candidate carrier and receiver](agent-candidate-export.md). Raw binary
files in the untracked JSON strings are not a lossless artifact transport.

### Explicit continuation of a closed-unresolved intent

An external controller's intent disposition may authorise one distinct successor after
the original and the most recent inspection incarnation are fenced. The host
can revalidate a stopped scope through `reconcile`; it serializes the current
machine probe and durable evidence commit against launch/replacement.

`GET /agent/workspace` returns the current Git head, bounded porcelain status,
binary-diff digest and inventory digest without reading authentication stores.
For the initial bounded recovery, continuation requires the unchanged source
baseline and clean status/diff. Unexpected retained work holds the transition
for assessment; it is never reset or discarded.

`POST /v1/environments/{id}/continuations` projects an external controller's existing
intent/lifecycle disposition into the retained broker journal. ZEO-RT is one
consumer example; standalone controllers do not need ZEO-RT's authorisation.
Pomar does not establish a separate approval authority. The owner supplies the disposition ID, intent and authority
references, responsible owner, original operation/input/thread/turn identities,
original and latest inspection fence operation IDs, the explicit new launch,
inventory digest, disk-recovery description, scoped external-action inventory,
and the new successor task. The host supplies its machine-owned fence receipts;
the generic agent proxy refuses caller-supplied continuation receipts.

The broker verifies both scopes and the new incarnation, rechecks the workspace,
and durably binds the successor ID/input digest before any inference. It retains
the original operation unchanged and saves its old thread association in the
binding. A new thread ID is appended before the successor's `turn/start`.
Repeated consumption checks journal health and retrieves the existing binding
under the same store lock. It succeeds after a durable binding even when subsequent
workspace changes exist; a failed save cannot produce a duplicate success receipt.
Changed input,
another disposition for the same original, unrelated uncertain tasks and journal
write uncertainty cannot create another task. Reconnect inspects the recorded
successor. There is no automatic replay of either attempt.

The display disposition is “original attempt unresolved; continued by …”, with
the successor outcome and actual recipient acknowledgements represented separately.
The repaired workspace remains recovery evidence from a damaged disk. This
supported development workflow does not establish abrupt-termination or power-loss
durability.

Pomar distinguishes its accepted input from agent acknowledgement and completed
work. An `accepted` operation is not a recipient acknowledgement in an external
message system. Codex `turn/completed` with `status=failed` or `interrupted` is
not task success. Progress comes from Codex app-server structured notifications;
permission requests are surfaced to the controller rather than approved by
the broker. Public execution approval is not organisational approval.

An adapter reader retains its launch incarnation rather than assigning delayed
notifications to the current journal incarnation. Stale notifications cannot
change task outcomes, acknowledgements, bindings or pending permissions. Bounded
`rejected_events` diagnostics retain source/current scope, method, matching
operation/thread/turn and payload digest without exporting raw provider content.
These diagnostics are not accepted work events or organisational answers.

The dedicated guest's root-owned login-shell profile preserves the intended
Codex and Go executable paths, including `/usr/local/go/bin`, after the base
image's login profile resets `PATH`. It leaves retained workspace and user files intact.

## Adapter

Adapter descriptions are reported facts, separate from execution authority.
The guest owner selects the provider, adapter/version, platform and executable;
the broker retains that executable's SHA-256 and a digest of its fixed launch
arguments and explicit environment before creating a thread. Resume refuses a
different selection. Raw launch-configuration and credential bytes do not enter
this binding. Mutable provider user settings and tools still require their own
effective-configuration compatibility evidence; this digest alone does not
establish that evidence.

The session report separates `owner_selection`, provider-specific negotiation
and `controller_qualified`. A descriptor's `ControllerQualified` claim is
ignored. Controller access requires a known qualified executable/version and
platform plus that provider's native negotiation. The current policy contains
the qualified Codex 0.160.0 Linux/arm64 and Darwin/arm64 executable pins. Claude
2.1.280 can report its own adapter without passing a Codex user-agent gate; its
controller bridge remains unqualified. An undescribed, unselected actor reports
unknown identity rather than implicitly becoming Codex.

A retained thread created before adapter binding was recorded cannot be
retrospectively attributed by this interface. Continue it with its previous
binary, or create a distinct environment; no automatic migration or task replay
is performed.

Codex app-server uses newline-delimited JSON RPC on stdio. Initialize the
connection, start/resume a non-ephemeral thread in `/work`, and retain the thread
and turn IDs. The broker remains connected when the controller disconnects.
After an actor replacement, resume the retained thread without replaying a
previous task. Authentication uses `account/login/start` with
`type=chatgptDeviceCode`; operator interaction and account entitlement remain
prerequisites for a real inference demonstration.

Pinned protocol source: Codex CLI 0.160.0 generated JSON schemas. See official
[app-server documentation](https://developers.openai.com/codex/app-server) and
[headless authentication](https://developers.openai.com/codex/auth).

## First assignment and acceptance

The initial assignment uses an isolated Pomar checkout: accept only ASCII
decimal listen ports in range 0 through 65535 in `cmd/pomar-shim`, retaining
zero for ephemeral listeners, with Go regression tests. The existing helper validates loopback hosts but does not
validate the supplied port. The guest may edit these files and run the focused
Go tests; it may not publish, deploy or alter a service.

Acceptance requires an actual authenticated Codex turn, code edits and tests
inside Linux, controller disconnection after submission, reconnect without a
duplicate turn, retained workspace/session evidence, stale-incarnation refusal
for reporting and execution, and a returned diff and test evidence. Synthetic
protocol tests and an unauthenticated process handshake are partial evidence.
Standalone acceptance precedes one external runtime binding. Claude Code is
the next first-class adapter; it is not a prerequisite for this demonstration.

## Versioned controller example

The companion [example packet](agent-environment-v1.example.json) contains
Go-neutral wire shapes. Its IDs and statuses are illustrative fixtures, not
evidence that a VM or authenticated task completed. Test/command results remain
structured actor events bound to the retained operation and source baseline;
the controller must assess them before claiming acceptance.

## Current-scope reconciliation and model observations

An authenticated owner can `POST /v1/environments/{id}/reconcile` over the
owner-only Unix socket with `operation_id`, `session_id`,
`expected_incarnation`, `launch_operation` and `fence_operation`. This is for
a retained uncertain scope. The request binds the immutable original launch
config, helper PID/UID/birth/command, and original revocation operation. Its
operation ID binds the full request; retries inspect the retained operation
and never repeat the probe. A new operation is needed for a later observation.

The development supervisor holds its exclusive data-root lock and the
environment operation lock during launch revocation, native machine probing,
and durable evidence/receipt commits. Forwarded adapter access and both
network proxies are closed. Any unresolved scope remains held. A completed
receipt separates `historical_execution: unknown`,
`current_scope: fenced_now`, workspace revocation and replacement eligibility.
The original stop action and legacy `vm-status.json` remain unchanged. New
incarnations write `vm-status-{incarnation}.json`; the immutable original
launch config is retained. Replacement adopts the current owner's artifacts
only after fencing, retaining the filesystem and stable session.

Coverage is specific to the supported direct Apple Virtualization backend:
the Swift helper constructs a `VZVirtualMachine` in its own process; guest
processes exist only inside that VM. It never launches guest work as host
children or mounts a host home/workspace. The kernel PID inventory and actual
executable paths cover every live process, including reparented Virtualization
XPC services. Any live Virtualization framework service with unexcluded
ownership holds reconciliation, even if it may belong to another workload.
The original helper is checked by UID and birth, and another instance of its
executable also holds. No process is signalled. Positive kernel zombie records
are terminal evidence; failed lookups are unknown.

The private workspace is a regular owner-only file. Every owned live process
is inspected for matching device/inode in vnode descriptors, fileports and
mapped regions, including processes whose parent has departed. Any remaining
holder or incomplete access lookup holds reconciliation. Two inventory passes
run while launch authority is revoked; the final file identity is checked
again. Other non-owner accounts have no access to this private data root.
The administrator and the authenticated service owner remain trusted host
principals; this does not claim to fence an adversarial administrator or
another owner acting outside the service. The service does not infer a VM stop
from an unchanged disk or process-name search. The probe receipt names its
coverage version, process/descriptor/region counts, observed inode, time,
identity, and specific unresolved paths. Bytes are synced before eligibility
is committed; journal failures hold further launches.

A session reports `requested_model` and `requested_provider` (empty means no
explicit override), and `model_observation.status` starts as `unknown`.
Responses from `thread/start` or `thread/resume` can record adapter-reported
`model`, `provider`, `thread_id`, `incarnation_id`, `operation_id`,
`observed_at`, and `source_method`. This is not independently verified backend
routing. Controller reconnects retain provenance; replacement clears the
current observation and requires its own resume observation. Resume has a
retained control operation and cannot silently retry an ambiguous dispatch.

`adapter.capabilities.version` is `pomar.codex-capabilities/v1`, with
`supported_operations`, `permission_response_kinds`, and
`permission_decisions`. Only per-request command/file approval responses
accept `accept`, `decline` or `cancel`. Other response kinds return HTTP 501
and `status: unsupported`, without answering the actor. No blanket approval is
implied. Host lifecycle control remains separate from the guest adapter API.

An adapter resume failure keeps the broker's inspection API available. Session
inspection then reports `recovery.status: held` and, for a protocol rejection,
the numeric `rpc_error_code`. Provider message/data bytes are never exported.
Original operation IDs, input hashes and uncertainty remain intact. New task
dispatch, permission responses and turn interruption are refused while resume
is unconfirmed. Actor departure also leaves durable inspection available;
neither a failed resume nor a missing rollout authorizes replay or a fresh
thread. Host execution fencing remains a separate lifecycle fact.
