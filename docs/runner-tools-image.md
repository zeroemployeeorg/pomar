# Local ARM64 runner tools

`node24-python314-chromium` names the guest-qualified local OCI layout
`localhost/pomar/runner-tools:20261007-v5`. It is not published in a registry.
Its index is `sha256:498c46febe123361a398475cdf47da02426ee1b6d6982ca15f190ac5bcc320bd`;
its ARM64 manifest is
`sha256:ab317bddf37bbc3bc03e5f51b40636512b9e2ef1266983791e8e439e24a4d7ae`.
The owner obtains the exact prepared layout through the private adoption
packet. Adding this catalogue entry does not install a daemon or admit a class.

```sh
pomar image load -root DATA_ROOT -host-bin SIGNED_HELPER \
  -layout REVIEWED_LAYOUT -catalogue node24-python314-chromium
pomar base build -root DATA_ROOT -host-bin SIGNED_HELPER \
  -image node24-python314-chromium
```

The explicit catalogue load derives the reference and ARM64 pin from code,
checks the staged layout's index, manifest and reference against that entry,
then invokes the existing checked loader. It does not replace an existing
store reference. Ordinary image loads still refuse catalogue repositories.
The local tag is a store lookup, not an execution authority: both the base
builder and guest helper refuse an index digest different from the catalogue.
An unavailable layout is not silently replaced with a registry image.

The image combines pinned official Node24.21.0/npm11.19.0 with GCC12.2,
Make4.3 and matching local Node headers; official CPython3.14.0's `/usr/local`
payload; uv0.12.23; authenticated Debian Chromium154.0.8037.92/runtime/font
data; and the locked Pillow12.3.0 ARM64 wheel. Public component identities
are in [runner-tools-image.json](runner-tools-image.json). No package or
maintainer script ran on the host. Package paths under `/bin`, `/sbin`,
`/lib` and `/lib64` are normalised to the merged `/usr` hierarchy. The
official Node layer's empty bundled npm `.npmrc` is omitted; the credential
checker remains unchanged.

The Python environment at `/work/.venv` contains only the pinned interpreter
links and Pillow. The immutable `uv` wrapper executes the pinned native
binary with frozen/offline operation, no interpreter downloads and that
fixed environment. This remains effective when a reviewed source's child
environment omits UV policy variables. Additional Python dependencies need
a newly reviewed image; there is no general runtime PyPI route.

On 2026-10-07 the exact image passed Pomar's OCI checker and a source-bearing
Linux guest check at UID/GID1000, 2 CPU/4 GiB and loopback-only networking.
The guest compiled and loaded a local-header N-API addon, retained the locked
Python environment through the source's `clean-all`, performed frozen/offline
checks with policy variables absent, rendered a pinned source font through
the source's actual Chromium module, captured PNG and converted it to WebP
with the existing Pillow program. Source stayed clean; the helper exited
and the VM/container were deleted. Original failures and success receipts
remain in the private recovery records.

These checks qualify the combined tools. They do not qualify a project's
complete CI gate, the named-source database addon fixture, permanent class
adoption, production rendering, runtime admission or deployment. A real
release still requires an independently pinned base/kernel/helper and
signer, reviewed source/input identity, measured shared capacity and the
project's complete fixed gate.
