.PHONY: build install test fmt vet test-tools test-all

# build produces a local ./fleetops binary (gitignored) — run it with
# ./fleetops, or `make install` to put fleetops on your PATH instead.
build:
	go build -o fleetops ./cmd/fleetops

# install builds and links fleetops into $GOBIN (or $GOPATH/bin, usually
# ~/go/bin) — make sure that directory is on your PATH. Same as
# `go install github.com/jitokim/fleetops/cmd/fleetops@latest` from
# outside a clone.
install:
	go install ./cmd/fleetops

test:
	go test ./... -count=1 -race

fmt:
	gofmt -l .

vet:
	go vet ./...

# test-tools runs the gates for the standalone modules under tools/, which the
# root `test` target CANNOT reach: each is its own Go module, so the root
# `go test ./...` stops at the module boundary and never descends. Without this
# the only way to see what CI sees is to cd into the module by hand.
#
# Deliberately mirrors dupdetect-test.yml's three steps exactly, including the
# gofmt form that FAILS rather than merely listing. The root `fmt` target above
# is `gofmt -l .`, which prints offenders and still exits 0 — fine as a local
# lister, useless as a gate, and CI does not rely on it (root-test.yml runs its
# own `test -z "$(gofmt -l .)"`). Repeating that shape here would have made
# `make test-tools` pass on code CI rejects, which is the specific way a local
# target becomes worse than no target at all.
test-tools:
	@for m in tools/*/; do \
		echo "==> $$m"; \
		( cd "$$m" && \
		  { test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }; } && \
		  go vet ./... && \
		  go test ./... -count=1 -race ) || exit 1; \
	done

# test-all is what to run before pushing: the root module and every tools/
# module, the same set CI covers across its two workflows.
test-all: test test-tools
