BINARY = mcp-telegram
MODULE = github.com/tolmachov/mcp-telegram

# Derive version metadata at build time so the binary self-reports a real
# version string instead of the "dev" default baked into internal.Version.
# git describe falls back gracefully: without tags it returns the short SHA.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(MODULE)/internal.Version=$(VERSION)

.PHONY: build lint fmt clean install

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)

install:
	go install -ldflags="$(LDFLAGS)" .
