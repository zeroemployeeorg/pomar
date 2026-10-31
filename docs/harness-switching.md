# Switching the actor while retaining its seat

`pomar actor-switch` switches an interactive seat between owner-configured
Claude Code and Codex profiles. The environment ID, stable session ID, retained
disk, workspace and each harness's native history/authentication stay in the
same box. Execution is stopped and fenced, then a new incarnation boots the
retained disk with the selected harness. This is a controlled VM restart, not
live replacement of an actor process inside a running guest.

## Prepare the two profiles

The owner supplies two `HostConfig.profiles` entries naming the same `seat`.
Both must use the existing box's source bundle/commit, base/image, kernel/init,
helper/guest/shim artifacts, CPU/memory. A harness
switch cannot silently reimage the seat, change its project or migrate its host.
Use a common base image provisioned for both harnesses.

The target profile names its own pinned agent archive/version, seat declaration
digest, egress allowlist and (for Codex) complete `seatLaunch` settings. Pomar
checks the target archive digest before stopping the current actor; the VM
helper checks it again before using it. Unknown profiles, another seat, missing
policy, wrong source/image, corrupt packages, qualification/headless profiles
and same-harness requests are refused before dispatch. Ordinary same-harness
replacement remains the existing `/replace` call without `target_profile`.

Profiles are owner-authorized deployment choices. This call does not rewrite
the organizational seat declaration or location record. The existing `seat up`
command still checks its canonical compiled declaration/profile; a switched
profile is inspected through `environments`/the environment API and attached by
its unchanged environment ID. Keep the declaration/catalogue consistent with
owner policy before using `seat up` to manage that selection.

## Execute or inspect

```sh
pomar actor-switch -root /owner/pomar \
  -environment seat-example -session SESSION -incarnation OLD_INCARNATION \
  -id switch-001 -profile example-codex

# If the response is lost, inspect with the original identity:
pomar actor-switch -root /owner/pomar \
  -environment seat-example -session SESSION -incarnation OLD_INCARNATION \
  -id switch-001 -status
```

The underlying owner-private API is:

```http
POST /v1/environments/seat-example/replace
Content-Type: application/json

{"operation_id":"switch-001","session_id":"SESSION","expected_incarnation":"OLD_INCARNATION","target_profile":"example-codex"}
```

The response is the environment record. Its original action contains a
`harness_switch` receipt with source/target profile and provider, session,
target configuration digest and selection phase. An unchanged retry returns
that receipt, even after restart or removal of the profile. Reusing the same ID
with a different profile, session, action or predecessor is refused. `-status`
never dispatches. CLI exit zero requires a completed replacement with a selected
harness receipt bound to the original request; a held/malformed/lost response
returns 1. Missing required flags return 2.

The sequence is durable intent, predecessor stop and revocation, independent
predecessor evidence, durable target selection, then successor admission/launch.
If fencing fails, the original profile remains selected and no successor starts.
If launch fails after selection, the chosen target and uncertain action remain
recorded; Pomar never silently starts the old actor again. A journal failure
latches the host against further launch. Inspect the original operation and
execution evidence; do not create a fresh switch ID to bypass uncertainty.

## History, credentials and the next turn

Claude retains its `.claude` home and seat supervisor state; Codex retains its
`.codex` home and root-owned thread/fence records. Switching back selects that
harness's retained history. Existing native ambiguity/configuration fences still
apply. No provider's transcript is converted or presented as another provider's
native thread, and cross-provider native continuation requests are refused.
Workspace files remain available to the successor. Runtime may deliver an
explicit handoff after checking the predecessor's unresolved operations; Pomar
does not replay a prompt, repeat tools or fabricate task completion.

An unused harness may need its own guest login. Credentials are not imported
from the host or copied from the other harness. As before, the seat is the trust
boundary: both coding actors run under its coding UID on its retained disk.
A completed switch means the replacement execution was launched, not that login,
model availability or an inference turn has succeeded. The target's normal idle
console/start-resume flow remains explicit. Other seats and disposable CI boxes
are unaffected.

## Qualification

The source tests cover selection/return selection, preserved disk identity,
incompatible inputs, incomplete fencing, lost acknowledgements, restart replay,
launch refusal after selection, journal failure, native-continuation refusal
and CLI request/response binding. Real Claude → Codex → Claude execution still
requires the native Apple/Linux guest gate, both provisioned archives and guest
logins. Source tests do not establish that live-host qualification or deployment.

A selected switch durably requires the retained disk on subsequent boots. If that disk disappears, the native owner refuses to clone a fresh base disk in its place.

Same-vendor reseating also requires the retained disk. CPU and VM memory allocations remain unchanged across switches. Retained actor
budget enforcement is a separate integration series and is not implemented by this slice.

## Upgrade and recovery limits before installation

After a selected switch, starts pin the selected harness binding. A changed client
archive/version/profile is held; this source does not implement an in-place
client upgrade. Keep the original installed profile to resume the retained seat.
An upgrade needs a separately reviewed owner operation; do not switch away and
back as a workaround or silently replace the pinned archive.

Before installation, the owner must read real journals for launch-operation and
current-incarnation fence coverage. Legacy journals with ambiguous multiple
launches or no bound fence remain refused. Preserve the original journal and
obtain owner reconciliation with positive process/disk evidence; never synthesize
receipts, remove the journal or create a fresh disk to bypass that refusal.
Predecessor probing runs under the host-wide lock; the native pilot must report
probe duration and effects on concurrent control requests. No native-pilot timing
or compatibility with existing live journals is claimed by source tests.
