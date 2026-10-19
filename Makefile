SHELL := /bin/sh
GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build check test integration fuzz

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o taper ./cmd/taper

check:
	@unformatted=$$(gofmt -l cmd internal); test -z "$$unformatted" || { echo "$$unformatted"; exit 1; }
	$(GO) mod tidy -diff
	$(GO) vet ./...

test:
	$(GO) test -race -count=1 ./...

# Mount permission is supplied externally; every tape endpoint is simulated.
integration:
	@dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT; \
	$(GO) test -c -race -o "$$dir/cli.test" ./cmd/taper && \
	$(GO) test -c -race -o "$$dir/fuse.test" ./internal/fusefs && \
	sudo unshare --mount env TAPER_FUSE_KERNEL=1 "$$dir/cli.test" -test.run '^TestReplicaSyncEndpointWorkflow$$' -test.v && \
	sudo unshare --mount env TAPER_FUSE_KERNEL=1 "$$dir/fuse.test" -test.run '^TestKernel' -test.v

fuzz:
	$(GO) test ./internal/format -run '^$$' -fuzz FuzzFrame -fuzztime 10s
	$(GO) test ./internal/format -run '^$$' -fuzz FuzzGroup -fuzztime 10s
