# pomar

Isolated, disposable Linux micro-VMs on Apple silicon: one VM per attempt, run by a Go coordinator and a thin Swift host built on Apple's [Containerization](https://github.com/apple/containerization) framework, on macOS 26.

Pomar's first job class is CI. Each attempt runs pinned source inside its own ARM64 guest. The guest holds no credentials, and the host enforces its network egress. The VM and its data are torn down when the attempt ends.

**Status:** early prototype. Nothing here is stable yet.

## Build and verify

Requires an Apple silicon Mac with macOS 26 and Go 1.27.

```sh
make verify
```

See `CONTRIBUTING.md`. Licensed under Apache-2.0.
