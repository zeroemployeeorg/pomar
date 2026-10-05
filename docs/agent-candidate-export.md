# Bounded release candidate return

`GET /v1/environments/{environment}/agent/result` returns a development diff
and untracked files as JSON strings. Raw binary wheel/sdist bytes cannot be
returned safely through those strings. Ignored `dist/` files are not exported.
This text-carrier contract works with the existing guest broker; it needs no
running environment replacement, host installation or publishing credentials.

Keep both original artifacts in the guest. Write standard padded base64 with
no whitespace or newline to untracked regular files `pomar-export/wheel.b64`
and `pomar-export/sdist.b64`. Write `pomar-export/manifest.json`:

```json
{
  "version": "pomar.candidate/v1",
  "artifacts": [
    {
      "kind": "wheel",
      "name": "example-1.0-py3-none-any.whl",
      "encoding_path": "pomar-export/wheel.b64",
      "size": 1234,
      "sha256": "<64 lowercase hex characters, hash of original binary bytes>"
    },
    {
      "kind": "sdist",
      "name": "example-1.0.tar.gz",
      "encoding_path": "pomar-export/sdist.b64",
      "size": 2345,
      "sha256": "<64 lowercase hex characters, hash of original binary bytes>"
    }
  ]
}
```

The existing export limits are 32 untracked regular files and 4 MiB total
untracked contents, plus a separate 4 MiB Git diff. This carrier reserves
256 KiB for the manifest, bounded test evidence and other new files. Artifact
base64 must total at most **3,932,160 bytes**, representing at most
**2,949,120 decoded bytes**. The manifest itself must be at most 16 KiB.
Check every path from `git ls-files --others --exclude-standard`, not just
the export directory. Verify carriers are untracked and not ignored. Keep
caches and virtual environments in established ignored locations. If the
measured candidate or complete inventory exceeds a bound, retain the work
and report the exact limitation for a separately arranged bounded transfer.
Do not truncate artifacts, delete source work, or substitute hash-only notes.

The controller saves the returned result and supplies a separate binding
from its retained submission, never copied from the returned result:

```json
{
  "environment_id": "example",
  "session_id": "session-example",
  "incarnation": "incarnation-example",
  "workspace_id": "workspace-example",
  "scope_id": "scope-example",
  "source_sha": "<40 lowercase hex characters>",
  "operation_id": "release-task-example",
  "input_sha256": "<64 lowercase hex characters from the retained task>"
}
```

Run the receiver from a source checkout, placing output under an existing
private directory owned by the controller:

```sh
go run ./cmd/pomar-agent-export -result result.json -binding binding.json \
  -output "$HOME/private-candidates/candidate-new"
```

The receiver requires the matching acknowledged, completed task. It validates
all identities, limits, filenames, canonical base64, decoded sizes and SHA-256
hashes before creating output. It writes actual wheel/sdist files with mode
0600 in a fresh mode-0700 directory, syncs them, and retains a bound receipt.
Existing output directories and symlinks are refused; partial write failures
are retained for diagnosis. The saved JSON input is capped at 32 MiB; a larger
event-rich result requires a separately reviewed transfer. Keep that result
private: it may include arbitrary guest output. The receipt contains hashes
verified against received bytes. It does not establish package compatibility,
successful project gates, recipient acknowledgement or publication approval.
