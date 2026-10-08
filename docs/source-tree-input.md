# Source-pinned tree inputs

`pomar-source-input` exports a local bare owner mirror without fetching,
changing refs, configuring a remote or copying Git history. An independently
reviewed full commit must be reachable from the exact named owner branch.
It includes every tracked regular file, executable modes and the dependency
lock. Git export-ignore/export-subst attributes cannot change the payload.
Symlinks, gitlinks, unsafe paths and an existing destination refuse.

```
pomar-source-input -mirror /owner/mirrors/engine.git \
  -ref refs/heads/main -commit REVIEWED_FULL_SHA \
  -output /private/preparation/engine.tar.gz \
  -require package.json,package-lock.json
pomar-source-input -verify /private/preparation/engine.tar.gz \
  -commit REVIEWED_FULL_SHA -require package.json,package-lock.json
```

The deterministic gzip tar contains the source tree and one reserved file,
`.pomar-tree-manifest.json`. Version1 records the exact commit object, Git tree
ID and every path/mode/size/Git blob ID/SHA256. Verification reconstructs the
complete Git Merkle tree and checks the commit against the independent pin;
the manifest's own claimed commit alone is insufficient. A single commit's
header may name parent IDs, but no parent objects/history or `.git` is included.
This is a tree/provenance proof, not a signature or a replacement for a trusted
owner pin. Verification neither extracts paths nor executes source.

The compressed cap is at most32MiB, expanded source at most256MiB and manifest
at most8MiB. Other inputs also count toward the manager's existing combined
32MiB admission cap; select a smaller export bound when needed. Required
dependency locks are included, not installed `node_modules`. Dependency
installation must use that tree's exact lock through the controlled registry
venue or a separately pinned offline image. Source owner gates must use the
manifest commit binding rather than requiring Git metadata in this input.

For ZBS this prepares the history-free engine payload for `/pomar/inputs/engine`.
It does not configure a class, provision mirror access, install a guest tool,
extract/admit an input, add a secondary npm lock to a running manager, or qualify
Node/npm equivalence. Actual owner source changes, current engine/content pins,
exact lock bytes, normal caller policy and dependency adapter remain necessary
before a real gate. The source owner retains acceptance of its gate results.
