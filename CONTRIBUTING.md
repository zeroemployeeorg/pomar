# Contributing to Pomar

Thank you for your interest. Pomar is early and changes quickly.

## Developer Certificate of Origin

Every commit must be signed off under the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Signing off certifies that you wrote the change, or otherwise have the right to submit it under this project's license.

Add the sign-off with `git commit -s`, which appends a line like this:

```
Signed-off-by: Your Name <you@example.com>
```

There is no contributor license agreement.

## License

Contributions are licensed under the Apache License 2.0 (see `LICENSE`). The Pomar name and any Pomar marks are not licensed with the code (Apache-2.0 §6).

## Before you open a pull request

Run `make verify`. It checks formatting, `go vet` and the tests. It also runs a hygiene check that rejects IP address literals and home-directory paths in tracked files. Use the documentation ranges (`192.0.2.0/24`, `198.51.100.0/24` and `203.0.113.0/24`) or `127.0.0.1` in examples, and `$HOME` in paths.

Keep each pull request to one concern.
