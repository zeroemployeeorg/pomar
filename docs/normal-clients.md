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
dispatch. It retains no login request bytes. The bounded HTTP response is in
an exclusive, durable 0600 `response.json` envelope (`Body` is base64); ordinary
stdout contains only action/ID, EUID, source/binary/PID metadata and outcome.
Do not publish the response envelope or private provider challenges.

A repeated identical call recovers its recorded response without networking.
Changed bytes or action under that ID conflict. After uncertain transport,
the client refuses blind mutation retry. `-reconcile` queries the original
host/agent operation with its session and incarnation, never a new ID. A
successful observation is explicitly `original-operation-observed`, not a
claim that dispatch/fencing/authentication completed. HTTP 5xx similarly needs
reconciliation; it is not non-acceptance. An explicit
`-retry-known-unaccepted` permits the identical original POST only after a
fresh `durable_non_acceptance` inspection. An unknown/refused inspection holds.
Neither observation nor retry invents an authentication response lost by the
server: inspect account/session state and the retained control evidence.

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
`source_base` fix the source materialization mode; callers cannot change it. The manager still applies its own kernel
peer/class checks. This does not expose its private owner socket/data/signing key.

One bounded JSON request arrives on stdin. Actions: `list`, `capacity`,
`signing-key`, `get`, `result`, `pins`, `log`, `output`, `start`. Reads of an
attempt use `attempt_id`; output additionally names one policy `output`.
Start supplies a normal `start` API object: the exact command/class/source
mirror/ref/SHA, input bytes plus digests, and output set. There is no arbitrary
path, shell command, class switch, stop/delete/drain/cache route or account
switch. Inputs remain within the existing 32MiB aggregate bound.

Before start, the caller locks and durably seals the attempt ID and canonical
request digest in its private records. It inspects that ID before submission;
existing attempts are observations, not new jobs. A repeat recovers the original
response or reconciles the same ID. Only explicit `retry_known_unaccepted`
plus a fresh 404 permits resubmission of those exact bytes. The server's own
unique attempt record remains authoritative. Responses are JSON envelopes with
HTTP status, caller UID, sealed digest and base64 body (including binary output).
Keep them in private files and inspect status; a CLI exit alone is not job success.

An administrator may install one fixed, digest-pinned, no-argument delegation
to this root-owned executable **as the dedicated caller**, never as root or
the manager owner. Prepare and validate that exact sudoers/configuration packet
before adoption. Do not add general sudo, another owner's socket access, a
new daemon or a guest credential. Actual provisioning is separate from source
merge. After installation, ordinary history/start/result retrieval needs no
administrator password. A remote controller still needs its agreed authenticated
host entry; a Unix socket or sudoers rule is not a new remote transport.
