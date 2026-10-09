# Pomar

Run isolated Linux jobs and development seats on an Apple silicon Mac. Pomar
uses one disposable micro-VM per job, a Go coordinator and a Swift host built
on Apple's [Containerization](https://github.com/apple/containerization).
Guests run pinned inputs without host credentials; the host controls egress,
capacity and retained results.

**Status:** early prototype, macOS 26 or later, Apple silicon. The release
pipeline is prepared here; the first binary release has not yet been published.
Until a release is published, the download commands below cannot succeed.

## Install a release

Users of a published release need no Git checkout, Go or Swift compiler. Choose
an explicit tag from [Releases](https://github.com/zeroemployeeorg/pomar/releases),
download that tag's `install.sh` and `SHA256SUMS`, and verify the installer before
running it. For example, **after `v0.1.0` is published**:

```sh
VERSION=v0.1.0
URL="https://github.com/zeroemployeeorg/pomar/releases/download/$VERSION"
curl -fL "$URL/install.sh" -o install.sh
curl -fL "$URL/SHA256SUMS" -o SHA256SUMS
awk '$2 == "install.sh" { print }' SHA256SUMS | shasum -a 256 -c -
sh install.sh "$VERSION"
export PATH="$HOME/.local/share/pomar/current/bin:$PATH"
pomar version
pomar help
```

The bundle includes the CLI, signed VM host, Linux shim, restricted clients and
these guides. Installation keeps versions side by side and activates only your
user tools. See [installation and rollback](docs/install.md).

## Run a job

Your host owner provides an approved control socket and job class. Installing a
client does not grant access to a manager or a development seat.

```sh
export POMAR_SOCKET=/path/to/approved/control.sock
pomar doctor
pomar status
pomar run -id hello-1 -class YOUR_CLASS -- /bin/echo hello
pomar jobs
pomar attempt get -id hello-1
pomar logs -id hello-1
```

`run` submits a job. A successful submission is not a successful job. After it
ends, retrieve and verify its signed result using the owner's independently
retained public key. Commands use JSON for job records. Explicit `-socket`
overrides the environment; an explicit `-root` selects that root's owner socket.
Pomar never guesses a production data root or socket.

For a repository job, name the owner's configured mirror and an exact source
commit with `-mirror`, `-ref` and `-sha`; you do not need a local clone.
See [operations, seats and result verification](docs/operations.md).

## Start a development manager

Installed releases also contain the tools to build a catalogue-pinned base,
fetch its pinned kernel, and start a foreground manager in an explicit private
root. They do not install a permanent service:

```sh
POMAR_DEV_ROOT="$HOME/.local/share/pomar-dev/example"
pomar server prepare -root "$POMAR_DEV_ROOT"
pomar server kernel -root "$POMAR_DEV_ROOT"
# Use the actual kernel digest printed by the previous command.
pomar server start -root "$POMAR_DEV_ROOT" -kernel-sha256 ACTUAL_DIGEST
```

This is a separate development instance. Capacity, images, source mirrors and
caller admission remain explicit host-owner decisions. A permanent deployment
continues using its reviewed installed binaries until its owner upgrades it.
See [development ownership](docs/operations.md#development-manager).

## Develop Pomar

Contributors build from source on Apple silicon with the pinned Go and Swift
requirements. `make verify` is the native merge gate. This is separate from
using an installed release. See [CONTRIBUTING.md](CONTRIBUTING.md),
[release preparation](docs/releases.md), [manager classes](docs/manager-classes.md)
and [agent environments](docs/agent-environments.md).

Licensed under Apache-2.0.
