# Normal local clients

Build these commands from a clean, reviewed commit with `go build -trimpath`.
Keep the source commit, clean VCS build metadata, executable SHA256, signature
metadata and gate receipt beside the stable installation. The owner command refuses unstamped or dirty builds. Do not use `go run`
or a cache executable for owner operations. Changing a path does not establish
security-alert clearance; security must correlate actual execution events.

## Agent owner

`pomar-agent-owner` uses the existing private `ROOT/host.sock` as its normal
owner. It checks directory/socket custody and the kernel peer UID before HTTP.
It neither elevates nor starts a manager. Supported actions are explicit:
`inspect`, `session`, `operation`, `agent-operation`, `start`, `stop`, `login`
and `login-complete`. Stop and login are different routes/operation bindings.
It does not submit tasks, grant authority, replace environments or retire data.

Supply `-root`, an existing private `-records` directory, `-environment`,
`-action`, and an explicit `-operation-id`. A mutation additionally requires
the original `-session`, `-incarnation`, and `-request FILE`. The request is a
single-link owner-only regular 0600 file in a private directory; it contains
`operation_id` and `expected_incarnation`. Only login completion additionally
accepts `login_id` and `code`. Sensitive values never belong in argv.

The records directory is 0700. Each ID is locked and bound to action,
environment, socket, session/incarnation and the exact request digest before
dispatch. Login request bytes are not journaled. Every dispatch, HTTP response,
transport failure and reconciliation has a separate immutable event ID and UTC
time. Sensitive bounded response bytes remain in private 0600 event envelopes;
stdout reports only metadata. Files and directories receive a full drive flush
on Darwin before acknowledgement. A crash during publication can complete only
an intact staged event whose identity matches the sealed request; partial or
conflicting evidence is retained and blocks progress.

Machine receipts separate `transport_outcome`, `operation_state`,
`session_state`, `response_available`, and `outcome`. HTTP200 carrying
`dispatching`, `acceptance_unknown` or an unknown original state is non-success.
A retained login control, including `sent`, cannot recreate its one-time login
response or establish the original completion result from current authentication.
Only a durably retained actual login response is reported as `response-available`;
completion additionally requires that response's explicit success boolean.
Session inspection remains a snapshot, not proof that another operation completed.

Repeated identical calls can recover an original response without networking;
`cached`, event identity and observation time identify historical data. Changed
bytes or action conflict. `-reconcile` always makes and appends a fresh original
operation inspection, including after HTTP409/5xx or a nonterminal observation.
Only fresh documented `durable_non_acceptance` plus explicit
`-retry-known-unaccepted` permits the same original ID and bytes to be dispatched.
No cached observation, missing local response or HTTP409 permits retry.

For a login completion, `-consume-request` removes the private request after a
successful completion response is durable. To recover that response later, omit
`-request`; the sealed hash still binds the original operation. A retry still
requires the exact original bytes. Without this explicit option, the owner
retains responsibility for removing the 0600 code file when it is no longer
needed. No provider credential store is read, copied or cleaned by this client.

Read-only inspection IDs are snapshots; use a fresh explicit ID for a fresh
session/inspect observation. A standalone operation inspection uses a separate
private receipt directory from the original mutation; `-reconcile` is the
usual way to inspect that mutation without changing its binding.

## Restricted CI caller

`pomar-ci-caller` accepts **no arguments**. Its effective UID selects
`/usr/local/etc/pomar/callers/UID.json`. The policy and all parent directories
must be root-owned and not writable by group/others; the file must be regular,
single-link and not a symlink. No environment variable selects policy or socket.

The policy names `caller_uid`, `manager_uid`, the restricted control `socket`,
private caller `records`, exactly one `class`, `mirror`, `ref`, fixed `command`,
exact `inputs` and exact `outputs`. `source_git`, `source_readonly` and
`source_base` fix the source materialization mode; callers cannot change it. `source_self_base`
requires Git and binds `base_sha` to the identical explicit source SHA. This
uses the existing head-and-base exporter with fully qualified branch policy;
it grants no independently selectable base or review-diff claim. The manager still applies its own kernel
peer/class checks. This does not expose its private owner socket/data/signing key.

One bounded JSON request arrives on stdin. Actions: `list`, `capacity`,
`signing-key`, `get`, `result`, `pins`, `log`, `output`, `start`. Reads of an
attempt use `attempt_id`; output additionally names one policy `output`.
Start supplies a normal `start` API object: the exact command/class/source
mirror/ref/SHA, input bytes plus digests, and output set. There is no arbitrary
path, shell command, class switch, stop/delete/drain/cache route or account
switch. Inputs remain within the existing 32MiB aggregate bound.

Before start, the caller locks and durably seals the attempt ID, exact original
request and canonical digest privately. Every inspection and dispatch/response
is appended under its own event identity. The caller compares an existing
attempt's class, source/ref/SHA/base/materialization, command, outputs, every
input size/digest and kernel-recorded submitting UID against its seal. A
collision is not this caller's admission. Nonterminal/uncertain observations are
freshly inspected on each call; historical201 admission responses are explicitly
cached. Admission is not successful job execution or signed-result verification.

Manager GET's documented404 means absence from its retained attempt table.
Only that fresh observation and explicit `retry_known_unaccepted` allow the
identical original request to be resubmitted; a409/503, cached404 or missing
response does not. Return envelopes include machine outcome, original attempt
state, transport state, caller UID, digest, observation identity/time and bounded
base64 body. HTTP refusals and uncertainty exit nonzero while still emitting the
machine envelope. Retain these envelopes privately. Result verification and
actual artifact acceptance are separate from admission/CLI success.

An administrator may install one fixed, digest-pinned, no-argument delegation
to this root-owned executable **as the dedicated caller**, never as root or
the manager owner. Prepare and validate that exact sudoers/configuration packet
before adoption. Do not add general sudo, another owner's socket access, a
new daemon or a guest credential. Actual provisioning is separate from source
merge. After installation, ordinary history/start/result retrieval needs no
administrator password. A remote controller still needs its agreed authenticated
host entry; a Unix socket or sudoers rule is not a new remote transport.

The response dimensions are separate: `response_available` describes the
current original-response observation; a fresh reconciliation does not become
that response. `retained_response_available` and `retained_response_event_id`
identify an earlier privately retained original HTTP response, including a
refusal, even after a new control observation. That pointer permits retrieval of
the original one-time login reply from its private event; it never establishes
success of the original operation or current authentication. `event_id` and
`observed_at` identify the current observation independently.
