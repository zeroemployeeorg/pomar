#!/bin/sh
# Maintainer-only: build a clean, explicit commit into a complete distribution.
# Run through the host's normal bounded native reservation, after make verify.
set -eu
version=${1:-}
out=${2:-}
[ -n "$version" ] && [ -n "$out" ] || { echo 'usage: sh tools/build-release.sh vMAJOR.MINOR.PATCH[-prerelease] ABSOLUTE_OUTPUT_DIR' >&2; exit 2; }
printf '%s\n' "$version" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9]+([.-][a-z0-9]+)*)?$/ {ok=1} END {exit !ok}' || { echo 'Use an explicit semantic release version.' >&2; exit 2; }
case "$out" in /*) ;; *) echo 'output must be absolute' >&2; exit 2;; esac
[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || { echo 'Build on an Apple silicon Mac.' >&2; exit 2; }
root=$(git rev-parse --show-toplevel)
cd "$root"
[ -z "$(git status --porcelain)" ] || { echo 'Refusing a dirty source checkout.' >&2; exit 1; }
commit=$(git rev-parse HEAD)
# Keep scratch/output outside the source checkout so VCS stamping stays clean.
[ ! -e "$out" ] || { echo 'Output already exists; use a new path.' >&2; exit 1; }
parent=$(dirname "$out")
[ -d "$parent" ] || { echo 'Output parent must exist.' >&2; exit 1; }
case "$out/" in "$root/"*) echo 'Output must be outside the source checkout.' >&2; exit 1;; esac
scratch=$(mktemp -d "$parent/pomar-build.XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOMAXPROCS=2
mkdir "$scratch/bin"
for command in pomar pomar-agent-owner pomar-ci-caller; do
 flags='-s -w'
 if [ "$command" = pomar ]; then flags="$flags -X main.version=$version -X main.sourceCommit=$commit"; fi
 CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -p 2 -trimpath -buildvcs=true -ldflags "$flags" -o "$scratch/bin/$command" "./cmd/$command"
done
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -p 2 -trimpath -buildvcs=true -ldflags '-s -w' -o "$scratch/bin/pomar-shim-linux-arm64" ./cmd/pomar-shim
# C/C++ dependencies also embed __FILE__; Swift-only maps leave those paths.
swift_release() {
 swift build --package-path host --force-resolved-versions --configuration release --jobs 2 \
  -Xswiftc -file-prefix-map -Xswiftc "$root=." \
  -Xswiftc -debug-prefix-map -Xswiftc "$root=." \
  -Xcc "-ffile-prefix-map=$root=." \
  -Xcc "-fdebug-prefix-map=$root=." "$@"
}
swift_release
host_bin=$(swift_release --show-bin-path)/pomar-host
cp "$host_bin" "$scratch/bin/pomar-host"
/usr/bin/strip -S -x "$scratch/bin/pomar-host"
codesign --force --sign - --entitlements host/pomar-host.entitlements "$scratch/bin/pomar-host"
codesign --verify --strict "$scratch/bin/pomar-host"
codesign -d --entitlements - --xml "$scratch/bin/pomar-host" 2>/dev/null | grep -q com.apple.security.virtualization
[ -z "$(git status --porcelain)" ] || { echo 'Source changed during build.' >&2; exit 1; }
[ "$(git rev-parse HEAD)" = "$commit" ] || { echo 'Source HEAD changed during build.' >&2; exit 1; }
# The manifest binds exactly the source and bundled tools; it is not a CI receipt.
go run -p 2 ./cmd/pomar-dist -version "$version" -commit "$commit" -bin-dir "$scratch/bin" -source "$root" -out "$out"
printf 'Prepared %s at %s; no tag, release, installation or service created.\n' "$version" "$out"
