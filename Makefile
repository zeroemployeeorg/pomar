# make verify is the merge gate. It runs on the maintainers' Apple silicon
# host at the exact head under review.
#
# Maintainers pass the private hygiene deny-list:
#   make verify HYGIENE_DENYLIST=<path> HYGIENE_REQUIRE=1
# Contributors can run plain `make verify`; the generic hygiene checks still run.

SHELL := /bin/sh
export GOTOOLCHAIN := local
export GOFLAGS := -mod=readonly

HYGIENE_DENYLIST ?=
HYGIENE_REQUIRE ?= 0
HYGIENE_COPY := .hygiene/denylist.txt

.PHONY: verify versions no-docker hygiene gofmt vet test swift-build swift-sign swift-test

verify: versions no-docker hygiene gofmt vet test swift-build swift-sign swift-test
	@echo "=== make verify GREEN ==="

versions:
	@echo "=== versions"
	@go version
	@sw_vers 2>/dev/null | sed 's/^/  /' || true
	@git rev-parse HEAD

# The host must not expose a Docker socket (mirrors the CI step's assertion).
no-docker:
	@echo "=== no-docker"
	@test ! -e /var/run/docker.sock || { echo "a Docker socket exists at /var/run/docker.sock"; exit 1; }

# The deny-list is copied in at build time and never tracked (.hygiene/ is ignored).
hygiene:
	@echo "=== hygiene"
	@rm -f $(HYGIENE_COPY)
	@if [ -n "$(HYGIENE_DENYLIST)" ]; then mkdir -p .hygiene && cp "$(HYGIENE_DENYLIST)" $(HYGIENE_COPY); fi
	@go run ./cmd/pomar-hygiene -denylist "$(if $(HYGIENE_DENYLIST),$(HYGIENE_COPY))" \
		$(if $(filter 1,$(HYGIENE_REQUIRE)),-require-denylist)

gofmt:
	@echo "=== gofmt"
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

vet:
	@echo "=== go vet"
	@go vet ./...

test:
	@echo "=== go test"
	@go test -count=1 ./...

# The Swift host. Its dependency graph is pinned by host/Package.resolved;
# --force-resolved-versions refuses to change it.
SWIFT_FLAGS := --package-path host --force-resolved-versions
HOST_BIN = $(shell swift build --package-path host --show-bin-path)/pomar-host
# The Command Line Tools keep Swift Testing outside the default search path.
CLT_FRAMEWORKS := $(shell xcode-select -p)/Library/Developer/Frameworks
SWIFT_TEST_FLAGS := $(if $(wildcard $(CLT_FRAMEWORKS)/Testing.framework),-Xswiftc -F -Xswiftc $(CLT_FRAMEWORKS) -Xlinker -F -Xlinker $(CLT_FRAMEWORKS) -Xlinker -rpath -Xlinker $(CLT_FRAMEWORKS))

swift-build:
	@echo "=== swift build"
	@swift --version 2>&1 | head -1
	@swift build $(SWIFT_FLAGS) --product pomar-host

# Ad-hoc signing with the Virtualization entitlement is part of the build.
swift-sign: swift-build
	@echo "=== swift sign (ad-hoc, virtualization entitlement)"
	@codesign --force --sign - --entitlements host/pomar-host.entitlements "$(HOST_BIN)"
	@codesign -d --entitlements - --xml "$(HOST_BIN)" 2>/dev/null | grep -q com.apple.security.virtualization
	@"$(HOST_BIN)" version

swift-test:
	@echo "=== swift test"
	@swift test $(SWIFT_FLAGS) $(SWIFT_TEST_FLAGS)
