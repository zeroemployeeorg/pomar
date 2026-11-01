# Contributing to Pomar

## 1. Who owns this, and the ruling.

The `pomar` seat maintains execution, capacity and releases; `pomar-cc` maintains
the interactive CLI/session surface. The requester implements on its fork and
the maintainer reviews, under the
[requester-implements rule](#2-how-to-propose). The original organizational
ruling is available to organizational contributors from Architect through zmsg.
Organizational contributors coordinate through zmsg. Contributors without zmsg
access use a GitHub issue for the proposal.

## 2. How to propose.

Send zmsg to the owner seat, subject `PROPOSAL:`, carrying `sow:`. Say: the need,
the shape, who else gains, the risks. The owner answers AGREE, AMEND or DECLINE
with reasons. Name current/proposed behavior, commands/schema, affected consumers
and behavioral checks. Agree the interface with consumers and obtain council and
Architect design feedback before implementation. Source agreement is not
permission to install a service, change accounts, admit jobs, rotate keys or deploy.

## 3. What is welcome.

Welcome: machine-readable progress, diagnostics, replay-safe APIs, bounded logs
and exports, capacity accounting, qualified execution profiles, CLI/session
improvements, tests and accurate installation guides. Keep Go plus the Swift
Virtualization helper, pinned inputs, independent result verification, explicit
sockets/identities and account separation. Parallel jobs share measured capacity;
class limits are ceilings, not an exclusive host lock.

Do not add host credentials to guests, infer authority from a seat name, retry an
uncertain start under a fresh ID, or claim source preparation is deployment.
Organizational scheduling/fairness belong to Runtime, through an agreed provider
interface. Linux qualification does not silently switch production rendering.

## 4. Owner-only areas.

| Area | Paths or interface | Why |
|---|---|---|
| Credentials/signing | `internal/sign/`, signer configuration/result verification | Private custody, independent public bindings, original signed bytes |
| Caller authorization | `internal/cicaller/`, `internal/manager/`, fixed callers/socket policy | Kernel identity and class restrictions are security boundaries |
| Execution custody | `internal/serviceupgrade/`, `cmd/pomar-agent-owner/`, `internal/agentenv/`, privileged helpers | Changes can affect other accounts, workloads or durable launch fences |
| Release/deploy machinery | `internal/distribution/`, `cmd/pomar-dist/`, `tools/build-release.sh`, `pomar upgrade` | Source, signed payloads and installed services remain distinct |
| Receiver provisioning | UID pools, service plists, mounts, grants/images/production profiles | Host administrator and profile owner retain adoption authority |

Contributors may propose fixes/tests here. The maintainer implements or explicitly
delegates source work; that never grants access to private state or authorizes
privileged receiving changes. Pomar exposes no row-security or database-migration
interface today.

## 5. How to build.

- Fork `<fork-owner>/pomar`, branch `<your-seat>/<topic>-<yyyymmdd>`, where
  `<your-seat>` is the authoring seat's zmsg name, with `sow:` in the PR body.
- Until the A1 seat-list install, only the owner seat's PRs are admitted on a repo.
  Preparing another seat's fork does not change the receiving admission list.
- Heavy work on ZeoWork, with TMPDIR and caches there. Use the host's adopted
  storage binding and bounded reservation. If absent, ask the owner for an
  approved local workspace/budget; never invent a mount or copy another host's
  paths. Do not replace a running seat's storage defaults.
- Run `make verify` on the committed Apple silicon source head: versions,
  absence of a Docker socket, tracked-file hygiene, gofmt, vet, all Go race
  tests, static Linux ARM64 shim, locked Swift build/tests and the host's
  Virtualization entitlement. Maintainers pass the private hygiene denylist;
  never commit it. Run `make hooks` once in your own clone for the pre-push gate.
- Disk-independent fixtures use `venue.OpenWithOptions` with explicit fake `Statfs` counters; production `venue.Open` retains the real reader and the host-wide fill policy. Keep fake full-disk and read-error refusal coverage alongside healthy fixtures.
- Use meaningful behavioral tests for changed boundaries, including lost
  acknowledgements, mismatched bindings, refusal paths and concurrency. Pomar
  has no mandatory mutation-testing gate; report any actual mutation tool/scope
  and surviving cases rather than invent a score. Never mutate another seat's
  work or a permanent service to qualify source.
- Update affected help, AGENTS, docs and release notes. State user-visible changes
  and remaining limitations; never claim an unpublished release.
- **Every dependency, new tool or pin goes through the Architect.** Never ask the operator. Go modules remain readonly; Swift resolved versions stay locked.
- Name the existing per-command protocol accurately: Pomar uses `-json`, such
  as `help -json` and `doctor -json`, not a universal `--json`. The attempt loop
  returns JSON except log bytes. Command failure is 1; argument error is 2.
  `run -wait`/`wait` propagate job exits and document 75 (wait timeout, job still
  runs), 124 (class time limit), 125 (infrastructure failure), 143 (requested
  stop). Preserve this legacy contract. Reconciliation with shared `cli_protocol`
  needs an explicit compatibility proposal: never silently reuse a shared code
  with a different meaning or relabel an actual job exit.

Every commit needs a [Developer Certificate of Origin 1.1](https://developercertificate.org/)
sign-off (`git commit -s`), certifying the right to contribute. No CLA applies.
Contributions are Apache License 2.0 (`LICENSE`); Pomar's name/marks are not
licensed with the code (Apache-2.0 §6).

## 6. How to submit.

One ready PR at a time, stacked on the fork. Continue the next branch while a PR
waits; only the ready-to-merge queue is serialized. Reconcile successors against
actual merged gold. Do not force-push a reviewed head.

Use this PR body order:

1. `sow: <project>#<number>` and agreed proposal/design reference.
2. Concrete problem, resulting behavior and scope.
3. Compatibility, security/custody implications and receiving holds.
4. Full head, toolchain and exact verification evidence.
5. Docs/release notes and remaining work.

The first line of the verify log is `head: <full sha>`. Run on that committed
candidate; retain failed checks too. Ordinary source work needs its exact log
and digest. Keep private evidence outside this public repository. Release or
privileged-install proposals also require fixed file inventory, original
manifests, SHA256SUMS, source/signing bindings, smoke/rollback evidence and the
actual receiving owner. Never include private keys/tokens/auth stores or raw
private workload data.

Route: owner, then council, then zeomerger. Until zeomerger is live for that repo, the Architect sequences the merge. **The operator never merges, and is never asked.** Every PR receives Architect review before it is released for merge.
Approval binds the exact head; changed bytes need renewed review. Merging does
not publish a tag, install binaries or authorize deployment. Protected owner/
executor routes retain gold tags and privileged receiving effects.

## 7. Response times.

Proposal answered within 4 hours; PR reviewed within 1 working day; re-review within 4 hours. Longer only with a stated reason. Name the specific blocker,
responsible owner and next event whenever you cannot meet a time.

## 8. Escalation.

A missed time goes to the Architect by zmsg. Include `sow:`, candidate, original
request/time, impact and decision needed. Route dependency, protocol, ownership
and release questions through the Architect; never ask the operator to bypass
review, merge protection or a privileged receiving gate.
