# Copyright 2026 The Loch.sh Authors
# SPDX-License-Identifier: Apache-2.0

BINARY  := loch-lambda-runtime
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
GOFLAGS := -trimpath
DIST    := dist

.PHONY: all build test cover vet lint release clean test-linux

all: vet test build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) ./cmd/$(BINARY)

vet:
	go vet ./...
	GOOS=linux go vet ./...

test:
	go test -race ./...

cover:
	go test -coverprofile=$(DIST)/cover.out ./...
	go tool cover -func=$(DIST)/cover.out | tail -1

# Runs the unit tests on Linux (process groups, subreaper, /proc).
test-linux:
	docker run --rm -v "$(CURDIR)":/src -w /src golang:1.27.3 go test -race ./...

# Static, reproducible binaries for the release architectures.
release: clean
	mkdir -p $(DIST)
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
			-o $(DIST)/$(BINARY)-linux-$$arch ./cmd/$(BINARY) || exit 1; \
	done
	cd $(DIST) && sha256sum $(BINARY)-linux-* > SHA256SUMS

clean:
	rm -rf $(DIST)
