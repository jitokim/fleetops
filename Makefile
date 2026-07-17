.PHONY: build install test fmt vet

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
