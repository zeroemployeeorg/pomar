# Health-read admission source preparation

`internal/healthreadlimits` supplies pre-parse byte, explicit-resource-set and
field-cardinality guards. It is not an authenticator and is not wired to a
network route. Its successful return never grants health access.

The selected limits are 64 distinct IDs of 1–128 ASCII bytes from
`[A-Za-z0-9._-]`, a 256-byte decoded protected header, 12,288-byte decoded
claims, a 20,480-byte complete Authorization value, a 16,384-byte HTTP/1
request line and a 32,768-byte whole header block. The latter two include
the line's CRLF; the block also includes the request line, field names,
separators, every CRLF and final blank line. These functions count the
supplied original bytes. They do not recover raw framing from parsed headers
or configure an HTTP server. Actual server and enabled HTTP/2 limits still
need transport-specific enforcement and boundary tests.

Empty resource sets refuse. Sets must match exactly, independent of order;
duplicates, extra or missing resources refuse the whole read. The authorised
set must come from independently verified owner state. Header/query guards
need all values from the parser, never a first-value selection that hides a
duplicate. Compact-encoding checks refuse padding, noncanonical base64url,
whitespace and size violations before later decoding work. They do not
verify a signature or a closed claims/header schema.

The full issuer profile, exact audience/scope/subject/resource registration,
root/key-manifest/freshness custody, clocks, restart/replay limits, valid
signed positives and producer identity remain separate requirements. A read
pass does not prove producer origin, working tools or effective live policy.
No key, issuer, endpoint, sync job, floor, credential or service is created
by this package. It does not qualify a seat, runtime admission or a pilot.

Maintainers can supply the independent synthetic vector directory through
`POMAR_HEALTH_VECTOR_DIR` when running the tests. The fixed manifest digest
and each stored/decoded file digest are checked before the 29 pre-parse cases.
The packet remains outside this public source tree. Those cases and the
public unit tests do not establish real HTTP-server or cryptographic
conformance. No usable token is recorded or returned by the tests.
