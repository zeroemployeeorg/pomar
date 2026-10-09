# Operate Pomar from installed tools

## Choose the manager

Use the exact approved control socket given by your host owner:

```sh
export POMAR_SOCKET=/path/to/approved/control.sock
pomar doctor -json
pomar status
pomar jobs
```

`doctor` checks the client, sibling bundled executables and a bounded Unix socket
connection. It does not authenticate a manager, verify its signature or
Virtualization entitlement, inspect provider state or submit a job. An OK
connection is reachability, not admission. A missing manager is an error; there
is no hidden fallback to a development or production instance.

Every attempt command accepts `-socket`. `POMAR_SOCKET` is its default. Explicit
`-root` without an explicit socket selects that root's owner socket even when
`POMAR_SOCKET` is set. Without either socket or root, the command refuses. Owner
commands and seat commands retain their existing explicit roots and custody.
Flags precede positional arguments or the job's `--` separator.

## Submit, inspect and verify a job

```sh
pomar run -id build-1 -class YOUR_CLASS \
  -mirror YOUR_CONFIGURED_MIRROR -ref refs/heads/main -sha FULL_COMMIT -- make verify
pomar attempt get -id build-1
pomar logs -id build-1
pomar attempt result -id build-1 > build-1.result.json
pomar result verify -reply build-1.result.json -public-key YOUR_PINNED_PUBLIC_KEY
```

The manager resolves and materializes its configured source mirror, so a caller
needs no source checkout. `-sha` binds the exact commit in the named ref's history.
A class chooses the guest image, resources, source policy and permitted callers.
Class refusal and resource refusal remain non-verdicts. A submission's exit0
reports admission only. Observe terminal state/exit code and verify the signed
result against the independently retained public key before accepting execution.
There is no automatic resubmission after an uncertain start; inspect the original
attempt ID. Reusing a name with different inputs is not a retry.

`logs` reads an ended job's retained bounded log. Add `-public-key` to verify the
signed result binding. Use `attempt output` for named outputs, and `help all` for
input/output, pins, stop and capacity options. Some routes are intentionally
unavailable through a restricted control socket. Installing tools does not
change those rules. The no-argument `pomar-ci-caller` has its own fixed owner-
installed policy; see [normal clients](normal-clients.md).

## Development seats

```sh
pomar seats -root YOUR_PRIVATE_HOST_ROOT
pomar seat status -root YOUR_PRIVATE_HOST_ROOT YOUR_SEAT
pomar seat attach -root YOUR_PRIVATE_HOST_ROOT -environment YOUR_ENVIRONMENT
```

Seat declaration, profile binding, location, owner receipts, source pin and
provider setup precede a seat start. `seat up` and `seat stop` are explicit owner
operations; they are not a public free-form container launcher. Attaching does
not alter organisational admission. Preserve an unresolved incarnation rather
than resetting its workspace or rebinding it to changed source.

## Development manager

Use a new absolute, dedicated root, separate from service data and provider
state. The foreground server commands require the root in argv; it cannot be
inherited from `POMAR_DATA_ROOT`. They refuse root execution and bind the host
and shim to the same installed release as the CLI.

```sh
POMAR_DEV_ROOT="$HOME/.local/share/pomar-dev/example"
pomar server prepare -root "$POMAR_DEV_ROOT"
pomar server kernel -root "$POMAR_DEV_ROOT"
pomar server start -root "$POMAR_DEV_ROOT" -kernel-sha256 ACTUAL_DIGEST \
  -class-name dev-example -class-concurrency 1
```

`prepare` builds the pinned default catalogue base; `-image NAME` selects another
catalogue image. `kernel` uses the catalogue-pinned kernel-fetch path. Both can
require network and disk space. They do not rebuild the Pomar implementation.
`start` runs the existing manager in the foreground. Its class/mirror/budget and
optional restricted control-socket options are the low-level manager options
shown by `pomar help all`. Concurrency1 in this example is a limit, not a measured
capacity qualification. Existing admission and host fill checks remain active.

In another terminal select the manager's owner socket explicitly with `-root`.
Wait for jobs to finish, then stop the foreground manager with Ctrl-C. Never use
this development convenience command to recover, restart or replace a permanent
service. A stopped manager does not itself establish that every helper or guest
has exited; inspect retained attempts and VM-orphan reports.
