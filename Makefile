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

.PHONY: verify versions no-docker hygiene gofmt vet test

verify: versions no-docker hygiene gofmt vet test
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
