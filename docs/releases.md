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
