# Copyright 2026 The Loch.sh Authors
# SPDX-License-Identifier: Apache-2.0

BINARY := loch-lambda-runtime
DIST   := dist
ARCHES := amd64 arm64

# Untrusted inputs: VERSION (a git tag in CI), SIGN, MINISIGN_KEY. Make
# never expands them: they are not exported to the recipes (make would
# expand them to build the environment), $(value …) takes them literally,
# and they reach the shell single-quoted, so "$(shell …)" or backticks in
# them run nothing.
unexport VERSION SIGN MINISIGN_KEY
# (unexport defines an empty variable on some versions of make: test the
# raw value, not the origin.)
ifeq ($(value VERSION),)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
ifeq ($(value SIGN),)
SIGN := none
endif
sh_quote    = '$(subst ','\'',$(value $(1)))'
version_sh := $(call sh_quote,VERSION)
sign_sh    := $(call sh_quote,SIGN)
key_sh     := $(call sh_quote,MINISIGN_KEY)
sign_value := $(value SIGN)

# check_version fails a recipe unless VERSION is a plain token (it ends
# up in the linker flags).
check_version = printf '%s' $(version_sh) | grep -Eq '^[0-9A-Za-z._+-]+$$' || { echo "invalid VERSION"; exit 1; }

# Reproducible builds: exact toolchain (go.mod requires at least it), no
# cgo, no path, no build id, no VCS stamp (the version is set explicitly),
# fixed CPU levels, checked module sums, and nothing from the environment.
export GOTOOLCHAIN  := go1.27.3
export GOFLAGS      :=
export GOSUMDB      := sum.golang.org
export GOPROXY      := https://proxy.golang.org,direct
export GONOSUMDB    :=
export GONOSUMCHECK :=
export GOPRIVATE    :=
export GOINSECURE   :=
export GOEXPERIMENT :=
export GOFIPS140    := off
BUILD_ENV := CGO_ENABLED=0 GOAMD64=v1 GOARM64=v8.0

# go_build builds the release binary for GOARCH $(1) into $(2).
go_build = $(check_version) && $(BUILD_ENV) GOOS=linux GOARCH=$(1) go build -trimpath -buildvcs=false \
	-ldflags '-s -w -buildid= -X main.version='$(version_sh) -o $(2) ./cmd/$(BINARY)

# Release tooling, run in containers pinned by digest (nothing installed),
# without network for the offline steps. SYFT, COSIGN and MINISIGN can be
# overridden to run installed tools (the GitHub release job passes the syft
# of anchore/sbom-action, same version); TOOL_DIST is then where they see
# dist/.
SYFT_IMAGE     := docker.io/anchore/syft:v1.57.0@sha256:3c70e6d5979364cefdc22b51617a78fd264574d3bd6b4f84a351204147025e2a
COSIGN_IMAGE   := ghcr.io/sigstore/cosign/cosign:v3.1.4@sha256:0f06154836d2de34f887a5c273506d52ca43edaa1d9e53e6510fa78a48f69c2d
MINISIGN_IMAGE := docker.io/jedisct1/minisign@sha256:ee60186c5d05e342e66cbc81133aa744d026752d909d8ea021a15c4d16b1193b
GO_IMAGE       := golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e

TOOL_DIST  ?= /dist
DOCKER_RUN := docker run --rm --user $(shell id -u):$(shell id -g) --cap-drop ALL \
	--security-opt no-new-privileges -v "$(CURDIR)/$(DIST)":/dist -w /dist
SYFT     ?= $(DOCKER_RUN) --network none -e SYFT_CHECK_FOR_APP_UPDATE=false $(SYFT_IMAGE)
COSIGN   ?= $(DOCKER_RUN) -e HOME=/tmp -e SIGSTORE_ID_TOKEN -e ACTIONS_ID_TOKEN_REQUEST_URL \
	-e ACTIONS_ID_TOKEN_REQUEST_TOKEN $(COSIGN_IMAGE)
MINISIGN ?= $(DOCKER_RUN) --network none --read-only $$([ -t 0 ] && echo -it) --platform linux/amd64 \
	-v "$$key":/key/minisign.key:ro $(MINISIGN_IMAGE)

# SIGN selects how SHA256SUMS is signed; none is active until the owners of
# the project choose (see docs/release.md):
#   none     - no signature (default)
#   minisign - with the key at MINISIGN_KEY, held by the maintainer
#   cosign   - keyless, from a CI with an OIDC identity
# REQUIRE_SIGNATURE=1 (set by the CI on tags) refuses SIGN=none.
# RELEASE_GIT_CHECK=0 skips the git checks (a CI checkout without .git).
REQUIRE_SIGNATURE ?= 0
RELEASE_GIT_CHECK ?= 1

SHA256    := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)
ARTIFACTS := $(foreach a,$(ARCHES),$(BINARY)-linux-$(a) $(BINARY)-linux-$(a).cdx.json)

# Targets run one at a time, in the order of their prerequisites.
.NOTPARALLEL:
.PHONY: all build test cover vet release release-check clean-dist binaries sbom checksums sign \
	reproducible clean test-linux integration

all: vet test build

build:
	$(check_version) && $(BUILD_ENV) go build -trimpath -buildvcs=false \
		-ldflags '-s -w -buildid= -X main.version='$(version_sh) -o $(DIST)/$(BINARY) ./cmd/$(BINARY)

vet:
	go vet ./...
	GOOS=linux go vet ./...
	go vet -tags integration ./integration/

test:
	go test -race ./...

cover:
	mkdir -p $(DIST)
	go test -coverprofile=$(DIST)/cover.out ./...
	go tool cover -func=$(DIST)/cover.out | tail -1

# Runs the unit tests on Linux (process groups, subreaper, /proc).
test-linux:
	docker run --rm --user $(shell id -u):$(shell id -g) -e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
		-v "$(CURDIR)":/src -w /src $(GO_IMAGE) go test -race ./...

# Runs the integration tests against the real AWS images (needs Docker).
integration:
	go test -tags integration -count=1 -timeout 15m -v ./integration/

# release builds the static binaries, their SBOMs and SHA256SUMS, then signs
# SHA256SUMS when SIGN is set.
release: release-check clean-dist binaries sbom checksums sign

# release-check refuses, before building: a VERSION that is not a release
# tag shape, a tree that is not exactly the tagged commit, an unknown SIGN,
# and an unsigned release when REQUIRE_SIGNATURE=1.
release-check:
	@printf '%s' $(version_sh) | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' \
		|| { echo "VERSION must look like v1.2.3 or v1.2.3-rc.1"; exit 1; }
	@case $(sign_sh) in none|minisign|cosign) ;; *) echo "SIGN must be none, minisign or cosign"; exit 1;; esac
	@if [ "$(REQUIRE_SIGNATURE)" = 1 ] && [ $(sign_sh) = none ]; then \
		echo "REQUIRE_SIGNATURE=1: set SIGN to minisign or cosign"; exit 1; fi
	@if [ "$(RELEASE_GIT_CHECK)" != 0 ]; then \
		git diff --quiet HEAD || { echo "uncommitted changes: commit, then release"; exit 1; }; \
		[ -z "$$(git ls-files -o --exclude-standard)" ] || { echo "untracked files: commit or remove them"; exit 1; }; \
		[ "$$(git describe --exact-match --tags HEAD 2>/dev/null)" = $(version_sh) ] \
			|| { echo "HEAD is not tagged with VERSION"; exit 1; }; \
	fi

clean-dist:
	rm -rf $(DIST)

binaries:
	mkdir -p $(DIST)
	$(foreach a,$(ARCHES),$(call go_build,$(a),$(DIST)/$(BINARY)-linux-$(a)) &&) true

# One CycloneDX SBOM per binary, from the module information it embeds.
# The random serial number and the timestamp are removed, so that the SBOM,
# covered by SHA256SUMS, is reproducible too.
sbom:
	for a in $(ARCHES); do \
		f=$(DIST)/$(BINARY)-linux-$$a.cdx.json; \
		$(SYFT) scan file:$(TOOL_DIST)/$(BINARY)-linux-$$a -o cyclonedx-json=$(TOOL_DIST)/$(BINARY)-linux-$$a.cdx.json || exit 1; \
		sed -E 's/"serialNumber":"[^"]*",?//; s/"timestamp":"[^"]*",?//' $$f > $(DIST)/sbom.tmp && mv $(DIST)/sbom.tmp $$f || exit 1; \
		! grep -Eq '"(serialNumber|timestamp)"' $$f || { echo "$$f is not reproducible"; exit 1; }; \
	done

checksums:
	cd $(DIST) && LC_ALL=C $(SHA256) $(ARTIFACTS) > SHA256SUMS

sign:
ifeq ($(sign_value),none)
	@echo "SHA256SUMS not signed (SIGN=none): see docs/release.md"
else ifeq ($(sign_value),minisign)
	@key=$(key_sh); case "$$key" in ''|*[!A-Za-z0-9._/-]*) \
		echo "MINISIGN_KEY must be a path made of letters, digits, '.', '_', '-' and '/'"; exit 1;; esac; \
	key=$$(cd "$$(dirname "$$key")" && pwd)/$$(basename "$$key"); test -f "$$key" || { echo "no key at MINISIGN_KEY"; exit 1; }; \
	$(MINISIGN) -S -s /key/minisign.key -m $(TOOL_DIST)/SHA256SUMS -t "$(BINARY) "$(version_sh)
else ifeq ($(sign_value),cosign)
	$(COSIGN) sign-blob --yes --bundle $(TOOL_DIST)/SHA256SUMS.cosign.bundle $(TOOL_DIST)/SHA256SUMS
else
	@echo "SIGN must be none, minisign or cosign"; exit 1
endif

# reproducible builds the release binaries twice, from two copies of the
# working tree in different directories with separate build caches, and
# compares their checksums.
reproducible:
	@set -e; tmp=$$(mktemp -d); trap 'chmod -R u+w $$tmp; rm -rf $$tmp' EXIT; \
	for copy in a b; do \
		mkdir -p $$tmp/$$copy; \
		tar --exclude=./$(DIST) --exclude=./.git -cf - . | tar -xf - -C $$tmp/$$copy; \
		for a in $(ARCHES); do \
			(cd $$tmp/$$copy && GOCACHE=$$tmp/cache-$$copy && export GOCACHE && $(call go_build,$$a,$$tmp/$$copy-$$a)); \
		done; \
	done; \
	for a in $(ARCHES); do \
		x=$$($(SHA256) $$tmp/a-$$a | cut -d' ' -f1); y=$$($(SHA256) $$tmp/b-$$a | cut -d' ' -f1); \
		echo "$$a $$x $$y"; test "$$x" = "$$y"; \
	done; \
	echo "reproducible: identical checksums"

clean: clean-dist
