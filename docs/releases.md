# Unreleased actor-switch integration slice

Owner-profile Claude/Codex switching now retains stable environment/session and
native histories while fencing the predecessor before durable target selection.
A missing retained disk refuses reseating instead of cloning a fresh disk.
No prompt/tool replay or cross-vendor native thread conversion is performed.
The actor-switch source at `bc6a0f1f` passed full `make verify` on Apple silicon
on 2026-10-31, including Go race checks and 89 Swift tests. Later changes
require their own committed-tree gate. Live Claude→Codex→Claude qualification
remains NOT_RUN. Installation must first check existing journals, report probe
time and complete a native round trip.

Predecessor eligibility now follows a durable current reconciliation operation.
A later uncertain observation supersedes older positive evidence even after an
ordinary stop or owner reload. Historical operation retries remain historical;
recovery requires a fresh explicit positive reconciliation. Legacy journals
without a current marker also require a fresh explicit observation.
Retained actor budgets and remote backends remain separate integration work.

# Release tools without coupling production to development

A release is an explicit version bound to a reviewed clean commit, complete
executable bundle and qualified native gate. Installed hosts keep that version
until their owners choose an upgrade. Checking out main, merging a PR or
building a candidate does not upgrade a deployment.

## Prepare a candidate

On an Apple silicon release builder, use a clean reviewed checkout. Run the
normal `make verify` gate through the host's bounded native reservation and
retain the exact commit, gate receipt and toolchain versions. Then, in the same
reservation policy, prepare an explicit version into a new output directory
outside the source checkout:

```sh
sh tools/build-release.sh v0.1.0-rc.1 /path/to/new/release-output
```

This is a maintainer operation; release users never run it. Go uses local
Go1.27, readonly modules, clean VCS metadata and two workers. Swift uses the
resolved Containerization0.45.0 dependency graph, release configuration and two
workers. Swift source/debug paths are remapped; local debug symbols are removed before
the host is signed with its Virtualization entitlement. The script
refuses dirty source, an existing output directory and a changed source HEAD.
It does not fetch/update source, tag, publish or install anything.

`pomar-dist` validates Mach-O arm64 executables and a static ARM64 Linux shim,
refuses embedded builder/home paths before creating output, constructs a fixed
allowlisted bundle, hashes every file, and produces a stable
ordered tar.gz with normalized timestamps and ownership. Identical input bytes
produce identical archives; Go/Swift/signing toolchain differences may change
executables. The manifest binds the archive's source commit and files. It is not
an independently signed CI result. The CLI prints the release version and source
commit; the Swift host's legacy version string is not a release identity.

The fixed bundle includes root `AGENTS.md` and the health, attempt-progress,
job-failure, controlled-service-upgrade and session/protocol guides. Their hashes and modes are
bound in the same release manifest as the executables. A modified or missing
declared guide refuses user installation before activation. Agents can discover
the main job loop, result verifier, artifact retrieval and owner operations
through `pomar help -json`; owner entries are discovery, not authorization.
Some commands still have unversioned JSON or human output: use each entry's
reported format and explicit JSON flag, rather than assuming every command is
machine-readable. Structured flag coverage is stated in the catalogue.

A candidate must also pass an installation smoke in a fresh user-owned prefix,
`version`, `help`, `doctor` against an approved test socket, repeated same-version
install, changed-byte refusal, upgrade and rollback. Retain archive hash,
manifest, OS/toolchain versions and the exact results. Do not activate a CI
service or admit a runtime actor to manufacture release qualification.

## Publish the first release

The repository has no published binary releases at this change. Preparation
alone cannot make download instructions work. The release owner must choose
an accepted clean commit/version and publish that commit's candidate bytes:

1. Complete substantive source review and exact native gate/installation smoke.
2. Freeze the version, source commit, archive SHA256 and file manifest.
3. Use the approved publisher to create the exact tag at that commit and a draft
   GitHub release, attaching the archive, `SHA256SUMS`, `release.json` and
   `install.sh`. Record hashes and source metadata in the release notes.
4. Independently re-download the assets; compare hashes and run the user install
   path on an approved clean host. Publish the release only after those checks.

Publication credentials and any workflow publisher are separate owner bindings.
This change requires no CI activation, authentication-scope expansion or new
service account. An unavailable publisher remains a concrete release action;
do not describe a local candidate as a downloadable release.

Do not overwrite an existing tag or asset/version with changed bytes. Publish a
new version. Developer ID signing/notarization and cross-host installation
qualification remain explicit release milestones. Production adoption follows
its existing reviewed owner procedure, with drain, preserved old binaries/data,
unchanged key/custody checks and real readbacks. User-tool installation is not
that procedure.

## Unreleased: Codex terminal custody fix-forward

The source candidate separates the tmux parser (guest UID1001) from the root
launch broker and the coding actor (UID1000), pins the parser executable,
anchors metadata reads and refuses startup without kernel TIOCSTI protection.
Only a Codex seat provisions that dedicated guest terminal identity; collisions
are refused. Existing Claude seats and non-seat app-server launches keep their
current setup. Login test fixtures now reap their own incomplete fake processes.
These source changes are not a signed release or an installed/native-qualified
Codex seat. Both source reviews and the guest security/lifecycle acceptance in
[codex-seats.md](codex-seats.md) remain required before adoption.
