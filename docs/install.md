# Install and upgrade user tools

Pomar distributions target **darwin/arm64 on macOS 26 or later**. A caller needs
an existing approved manager. A development owner can start a separate instance
with the same bundle. Neither route needs the Pomar source repository.

## Download

Select an explicit published tag, including its prerelease suffix when present.
There is no implicit `latest`, automatic updater or mutable production checkout.
Each release contains:

- `pomar-VERSION-darwin-arm64.tar.gz`: complete tools and public guides;
- `SHA256SUMS`: archive and installer SHA256 values;
- `install.sh`: version-selecting user installer;
- `release.json`: source commit, platform, minimum OS and every bundled file hash.

Download `install.sh` and `SHA256SUMS` from the same tag, check the installer hash,
review the script, then run `sh install.sh VERSION`. The script downloads and
checks that tag's archive over HTTPS and calls its bundled `pomar install`.
Checksums fetched beside an artifact establish integrity against those bytes;
they are not an independent signing-key or notarization proof. The VM host has
its required ad-hoc Virtualization entitlement. Developer ID signing and
notarization are not yet supplied by this pipeline. Do not disable Gatekeeper
or strip quarantine automatically to make an installation succeed.

The installer uses your `TMPDIR`; allow space for the compressed archive and
its extracted tools. It removes its own completed download scratch. It does not
download guest images, kernels, provider logins or another user's data.

## Install an already downloaded bundle

After verifying and extracting a release archive:

```sh
./pomar-v0.1.0-darwin-arm64/bin/pomar install \
  -bundle "$PWD/pomar-v0.1.0-darwin-arm64" \
  -prefix "$HOME/.local/share/pomar"
export PATH="$HOME/.local/share/pomar/current/bin:$PATH"
pomar version
```

Use a private user-owned prefix with no writable-by-others parents or symlink
components. The installer verifies the complete manifest and refuses missing,
changed, extra or symlinked files. It copies into staging, verifies the copy,
retains `releases/VERSION`, then atomically updates `current`. A conflicting
version is never overwritten. The prefix serializes installers with an explicit
lock; an interrupted installer leaves its lock for the owner to inspect.

Changing PATH selects your CLI. It does not replace a running process or change
any installed daemon, CI account, signing key, caller rule, source profile or
runtime actor. Permission refusal is a host-owner decision, not an invitation
to use sudo or expose an owner socket.

## Upgrade and rollback

Install the new explicit tag into the same prefix. Old versions remain intact.
To select a previous retained version, run its own installer against its bundle:

```sh
OLD=v0.1.0
PREFIX="$HOME/.local/share/pomar"
"$PREFIX/releases/$OLD/bin/pomar" install \
  -bundle "$PREFIX/releases/$OLD" -prefix "$PREFIX"
```

Rollback selects tools for future commands. A running manager and its data are
unchanged. No data-format downgrade compatibility is implied. Permanent service
upgrade/rollback requires its separate owner-controlled drain, preservation,
key/custody checks and exact-binary installation procedure.

Completed release versions may be removed only after the owner verifies that
no running process, receipt or rollback requirement references them. The
installer performs no automatic pruning and never removes retained environments.
