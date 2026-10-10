# Job failure diagnostics

The VM helper polls for the command's exit with a one-second deadline. A typed
timeout retries after a short delay; a disconnected VM or agent fails the
attempt and starts cleanup. A process error is never evidence of a successful
command. Guest outputs cannot be recovered after the VM has disappeared.

Before deleting its container, the helper keeps at most the final 1 MiB of its
own boot log in the private attempt state directory as `boot.log`. Status
metrics report preservation, byte count, offset and truncation. Missing or
partial diagnostics are explicit. The copy refuses symlinks, special files
and an existing destination, and creates a file with mode 0600. It does not
publish the boot log through the control API or include it in a signed result.

The npm relay writes correlated JSON lines with schema
`pomar.npm-request/v1`: `received`, `cache`, `upstream_started`,
`upstream_connect_started`, `upstream_connected`, `upstream_first_byte` and
`completed`. Each includes a request number, UTC time and elapsed milliseconds;
completion includes HTTP status and bytes. An unfinished request has a start
without a completion. Connection timing can be absent when a connection is
reused or when a custom transport does not implement HTTP trace hooks.

The historical completion-line format remains alongside the events. Parsers
must distinguish JSON events from completed responses and must not count
events as additional downloads. Only locked tarball paths and the fixed
negative routes `/npm` and `/-/ping` appear in diagnostics. Other paths,
queries, absolute URLs, headers and unexpected methods are redacted.
Upstream downloads remain streamed to a temporary file and checked against
the lock's SHA512 before serving. A disconnected request cancels its upstream
request; diagnostics do not widen the registry allowlist.

These records do not expose the vminitd agent's memory pressure or live vsock
connection count. Guest job cgroup statistics and relay completion counts
are different measurements. Qualification still requires the actual terminal
result, expected outputs and their verified hashes, rather than a wrapper's
exit code or printed `PASS`.

The registry diagnostic checker takes a private result directory, a separately
reviewed expectation JSON, and that expectation's independently retained SHA256:

```sh
node scripts/qualify-registry-diagnostic.mjs RESULT_DIR EXPECTATION_JSON EXPECTED_SHA256
```

The `pomar.registry-expectation/v1` document binds `attempt`, `source`,
`command`, host `uid`, `guestUid`, original `lockSha256`, the six classified
`npmConfig` fields, and exactly 32 distinct `selectedRoutes` with locked
`package`, public registry `resolved` URL and SHA512 `integrity`. Both probe
reports must contain the same expected package/path/observed SRI set, config,
lock digest and B1 snapshot. Each request includes its computed `integrity`,
not just an `integrityMatch` boolean. A duplicate set or a report from another
lock cannot pass even when its output hashes are internally consistent.

Input files are bounded regular files; the checker refuses symlinks and never
prints refused report content. This checker does not verify a manager signature
or qualify production. Independently verify any signed result before using it.
The lifecycle-disabled install verdict remains limited to the diagnostic.
