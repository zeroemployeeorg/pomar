# Upgrade an existing service

`pomar install` installs a complete user-prefix release. `pomar upgrade` is a
separate administrator operation for an **already provisioned** macOS service.
It does not create accounts, install a daemon, change grants, create a class,
submit a job or admit a runtime actor.

The v1 service profile binds the existing `_pomar` owner and `_profrodci` caller,
fixed manager label, binary paths, public caller policy, narrow grant, classes
file and unchanged launch plist. It refuses another deployment layout. New
host provisioning and more profiles require their own implementation/review.

## Frozen payload

The release owner prepares four regular files: `upgrade.json`, `pomar`,
`pomar-host`, and `pomar-host.entitlements`. The separately retained SHA256 of
`upgrade.json` anchors every file's exact hash and size, the baseline and target
manager/helper source commits, host and operator/account UIDs, existing install
record, manager public-key ID, and retained shim/caller/policy/grant/classes/plist.
The manifest contains roles, never arbitrary target paths or shell commands.
Duplicate fields, unknown fields and excess nesting are refused.

For `internal_certificate`, both target binaries must have the manifest-bound internal certificate and the
fixed product identifiers `ac.zeo.pomar.pomar` / `ac.zeo.pomar.pomar-host`.
For either policy, the host must have exactly the reviewed Virtualization entitlement; the
coordinator must have none. An ad-hoc development signature is insufficient for an internal release.
The separate `development_transition` policy requires structurally verified
ad-hoc signatures on both the installed baseline and exact target, with no
claimed certificate. It must be explicitly reviewed for a development-service
transition; it is never silently selected or represented as an internal release.
The upgrade command verifies signatures; it does not implement another signer,
read a credential store or unlock a keychain. Missing internal signing capability blocks an internal release; it does not
invent a new gate for an explicitly reviewed development transition.

## Product command sequence

These are **command shapes**, not a released host-specific recipe. Substitute
only values from an actually reviewed and signed release packet. The root-owned
product CLI itself must be the reviewed qualified build, independently verified
by the administrator. Do not execute a CLI from a seat-writable directory as root.

```text
sudo /path/to/verified/pomar upgrade stage -bundle RELEASE_DIR -manifest-sha256 SHA256
sudo /path/to/verified/pomar upgrade check -bundle RETURNED_ROOT_STAGE -manifest-sha256 SHA256
sudo /path/to/verified/pomar upgrade apply -bundle RETURNED_ROOT_STAGE -manifest-sha256 SHA256
```

Stage copies only frozen payload files into a private root-owned stage and
verifies the signatures there. It does not alter the service. Retain its path
on failure. Check creates no lock, stage, journal or archive and sends only
read-only owner/caller requests. It checks the actual installed baseline and
process identity, no live attempts or writing descendants, no unresolved owner
VM orphans, the independently bound public key and fresh actual caller history,
class/budget/key readbacks. Existing legacy-class access must remain refused.

Apply acquires the product operation lock and repeats the checks. It checks
space before archiving the two old binaries and exact install record in a private
root-owned archive. It records durable intent before each effect. The existing
manager's atomic owner drain prevents new admission; only the matched idle
manager is unloaded. The control directory is closed across restart. No other
process is signalled. Only the two pinned binaries and install record change.
Prior concurrency/reboot/check qualifications are explicitly invalidated before
replacement and never inferred from a successful restart.

After restart, the new manager's drain must be confirmed before reopening the
control path for actual caller GET readbacks. The command verifies installed
file hashes/signatures, retained files, every terminal history record (including
unknown evidence fields), public-key binding and owner/caller class/budget
identity. Only after all checks does it lift admission's drain. A failed check
retains its journal/archive and holds admission for reconciliation. Ordinary
host reservation/admission and full native site qualification remain separate.

## Recovery and rollback

An error is an incomplete transaction, not evidence that nothing changed.
Preserve the reported stage, archive and journal. Do not replay apply or delete
its archive. Read-only recovery inspection and explicit rollback use the same
manifest and original archive:

```text
sudo /path/to/verified/pomar upgrade check -bundle ROOT_STAGE -manifest-sha256 SHA256 -archive ORIGINAL_ARCHIVE
sudo /path/to/verified/pomar upgrade rollback -bundle ROOT_STAGE -manifest-sha256 SHA256 -archive ORIGINAL_ARCHIVE
```

Rollback refuses a different manifest, arbitrary archive, unknown installed
binary/record bytes, active work or uncertain process identity. It restores only
the archived baseline binaries and exact old record, restarts the unchanged
service, repeats post-install readbacks and then reopens admission. It does not
rewrite result history or replace signing keys, images, kernel/rootfs, caller,
policy, grants, classes or launch configuration. If those bindings changed or a
new job was admitted, stop and reconcile with the service owner.

Source/unit qualification of this implementation does not demonstrate a real
root-owned service transition. An installable recipe also needs the exact
sealed payload under the applicable reviewed signing policy, qualified CLI, current owner observations,
review and an attended administrator execution result. Retain those receipts
before updating any dependent release acceptance policy.
