sow: pomar-guest-SOW-01

# Isolated fixed-smoke protocol source

This first source slice implements the host's bounded report decoder and per-run
cleanup refusal model. It does not boot a VM, prepare a root filesystem, install
a guest unit, open a vsock listener, remove files, sign a result or admit jobs.
It is separate from the existing helper and Package.swift. The reviewed design
record is routed through ledger by the Architect; the implementation PR follows
Pomar #136. No publication or receiving authority is inferred here.

The agreed transport carries a four-byte big-endian body length followed by one
UTF-8 JSON object, then EOF. Length must be 1 through 1,048,576, checked before
allocation or body read. The host requires a single deadline of at most 120 s
covering header, body and EOF; extra bytes, a second reply or missing EOF fail.
Unknown fields, duplicate keys (including escaped equivalent keys), invalid
types and trailing JSON bytes are rejected. Decode errors contain no guest text.

The closed body has these fields:

| Field | Value |
| --- | --- |
| `schema` | `pomar.guest-smoke/v1` |
| `run_id`, `vm_id` | Exact expected binding, each 1–128 ASCII letters, digits, `.`, `_`, or `-` |
| `manifest_sha256` | Exact expected 64-character lowercase hexadecimal SHA256 |
| `evidence_source` | `SELF_REPORTED` |
| `outcome` | `pass` iff all checks are true, otherwise `fail` |
| `checks` | Exactly one check for each required name below |

Each check has exactly `name` (string), `ok` (boolean), and `detail` (nonempty
UTF-8 string, at most 4096 bytes). Names are `os`, `kernel`, `tools`, `pid1`,
`settled_unit`, `main_pid`, `start_timeout`, and `unit_cleanup`. Detail is
diagnostic content, not a verified identity, trusted terminal result or host
instruction. The eventual fixed producer must collect the actual required
OS/arch/kernel/tool hashes, systemd executable identity, settled unit properties,
live MainPID/executable/start/cgroup identity, timeout result and unit absence.
The schema does not grant a generic command interface or replace that evidence.

The lifecycle permits mutable-data removal only after explicit positive VM-stop
and executor-reap observations. Unknown quiescence or remaining mutable paths
quarantine the run; neither a quarantined nor completed instance may boot again.
This is an in-memory per-run model, not a durable cross-process admission lock.
The eventual backend must supply real observations, global custody, bounded
180 s boot/60 s cleanup, precise run-owned path containment, retained immutable
inputs/proof and crash recovery. No such backend is qualified by these tests.

After a finite native build allocation and existing tool/dependency closure are
granted, compile the isolated synthetic test executable explicitly:

```sh
swiftc host/GuestSmoke.swift tools/guest-smoke/ProtocolTests.swift -o "$TASK_TEST_BIN"
"$TASK_TEST_BIN"
```

`TASK_TEST_BIN`, TMPDIR and caches must point into the approved task child.
These synthetic refusal tests do not replace committed-head `make verify`,
Linux guest qualification, artifact signature verification or teardown evidence.
No build or guest success is claimed merely by adding this source.

The successor suite schedules fragmented header/body delivery and adds invalid
descriptor/limit and owned-pipe read-error cases. A timeout can occur before the
body is reached, so cross-stage deadline coverage remains INCONCLUSIVE without
observed read-stage evidence. The finite writer joins before descriptors close.

The previous SIGUSR1 fixture is removed: successful signal sending did not prove
handler delivery before restoration or that poll/read returned EINTR. This suite
installs no signal handler, changes no signal mask and targets no receiving
thread. The production EINTR retry branch remains NOT COVERED by these tests.
No new native run is requested by this source change. Native logs must retain
all failures and fixture/scheduling limits. The original93 synthetic PASS
qualifies neither this successor's compile/run behavior nor its added coverage.

The separate manifest/bootstrap proposal follows the records review route.
A canonical record_ref will be cited after its independent review and carry;
no schema or executable bootstrap is adopted by this code change.
