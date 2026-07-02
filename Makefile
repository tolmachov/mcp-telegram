BINARY = mcp-telegram
MODULE = github.com/tolmachov/mcp-telegram

# Derive version metadata at build time so the binary self-reports a real
# version string instead of the "dev" default baked into internal.Version.
# git describe falls back gracefully: without tags it returns the short SHA.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(MODULE)/internal.Version=$(VERSION)

.PHONY: build lint fmt clean install test test-integration

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

# Unit tests. Integration tests are guarded by the `integration` build tag and
# are excluded here; run them with `make test-integration`. Note: on macOS the
# secret/session-store tests read the Keychain and may raise a one-time access
# prompt for a freshly built test binary — allow it once and it won't recur.
test:
	go test ./...

# Integration tests hit a real Telegram account and need credentials plus the
# TEST_* environment variables (see .env.example). They no-op/skip without them.
test-integration:
	go test -tags integration ./test/...

clean:
	rm -f $(BINARY)

install:
	go install -ldflags="$(LDFLAGS)" .
