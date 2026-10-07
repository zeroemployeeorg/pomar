# Multiple CI classes

One manager can serve several classes within its existing host budget. Start
it with `-classes-file PATH` instead of the `class-*` flags. The file is a
trusted owner configuration, not a client request. It must be a regular file
with no group/other write permission, at most 1 MiB, using schema
`pomar.manager-classes/v1`. Unknown fields, duplicate JSON fields and duplicate
classes or caller UIDs are refused. Accounts must already exist.

```json
{
  "schema": "pomar.manager-classes/v1",
  "classes": [
    {
      "name": "ci-example",
      "image": "node24-full",
      "vcpu": 2,
      "memory_mib": 8192,
      "disk_peak_gib": 8,
      "concurrency": 1,
      "time_limit_s": 10800,
      "log_cap_bytes": 16777216,
      "callers": ["example-build"],
      "source_mirrors": ["example"],
      "source_ref": "main",
      "command": ["make", "verify"],
      "job_user": true,
      "go_proxy": false,
      "npm": true,
      "npm_lock": "package-lock.json"
    }
  ]
}
```

The first class is the default. Every configured class requires explicit
callers and source mirrors, a catalogue image and positive CPU, memory,
concurrency and time limits. Mirror URLs still come from the owner's
`-mirror-url NAME=URL` flags. A source mirror outside a class's list is refused
before syncing it or reserving capacity. Classes on a multi-class control
socket cannot share unrestricted source access merely because their callers
can connect to that socket.

`source_ref` additionally requires that ref and an explicit full commit SHA
at admission. `command` restricts the exact argument list of a fixed workload.
Without these fields, a class retains the normal pinned-source and command
interface within its declared mirrors. An omitted `go_proxy` is false: the
class does not receive the general Go module proxy. `npm` uses the existing
hash-locked registry derived from the admitted source's lockfile. This does
not grant public HTTP access or download arbitrary native-addon headers.

Class callers are kernel peer UIDs. Distinct projects sharing one OS account
cannot be separated by a seat name. The owner's socket remains unscoped and
must stay private. Source and output verification still belong to the consumer;
configuration does not make an unqualified image or provisional resource
figures measured, and does not install or upgrade a permanent daemon.

Each class verifies its own base at manager startup. All selected bases and
the shared kernel remain pinned against eviction. Aggregate CPU/memory/disk
admission and per-class concurrency limits continue to apply. Project-specific
external reservations must also be reconciled by the host provisioning owner.
