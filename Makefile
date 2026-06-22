.PHONY: build run test fmt fmt-check lint

# This module's own package dirs (go list stops at module boundaries, skips deps).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)

# Build the SWE swarm binary.
build:
	CGO_ENABLED=0 go build -trimpath -o bin/swe ./cmd/swe

# Run the TUI directly. Loads .env (if present) so LLM_API_KEY and friends are
# exported for the process.
run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/swe

test:
	go test -race ./...

# Format this module's Go files in place.
fmt:
	gofmt -w $(GO_DIRS)

# Fail if any Go file is not gofmt-clean.
fmt-check:
	@unformatted=$$(gofmt -l $(GO_DIRS)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

lint: fmt-check
	go vet ./...
