# Releases

A release is two static binaries (`linux/amd64`, `linux/arm64`), one
CycloneDX SBOM per binary, `SHA256SUMS` over all of them, and a signature of
`SHA256SUMS`. Consumers verify the signature, then the checksums.

```sh
make release VERSION=v1.0.0            # binaries, SBOMs, SHA256SUMS (unsigned)
make release VERSION=v1.0.0 SIGN=…     # … and the signature (see below)
make reproducible VERSION=v1.0.0       # two builds in two directories, same sha256
```

Before building, `make release` (`release-check`) refuses:

- a VERSION that is not `vX.Y.Z[-suffix]`;
- uncommitted changes, untracked files, or a HEAD that does not carry the
  tag VERSION (`RELEASE_GIT_CHECK=0` skips these git checks, for a checkout
  without `.git`);
- an unknown SIGN, and SIGN=none when `REQUIRE_SIGNATURE=1` (the CI sets it
  by default on tags: once the signing method is chosen, an unsigned
  release fails instead of slipping through).

VERSION, SIGN and MINISIGN_KEY are untrusted (a tag name may contain
`$(…)`, backticks or `;`): the Makefile never expands them nor exports
them, and passes them to the shell single-quoted;
`internal/release/makefile_test.go` checks that such values run nothing.

## Reproducible build

- Toolchain pinned: the Makefile exports `GOTOOLCHAIN=go1.27.3` (`go.mod`
  requires at least go 1.27.3); the go command fetches that exact toolchain
  if needed.
- `CGO_ENABLED=0`, `GOAMD64=v1`, `GOARM64=v8.0`, an empty `GOFLAGS`,
  `-trimpath`, `-buildvcs=false`, `-ldflags '-s -w -buildid=
  -X main.version=<VERSION>'`: no path, build id, VCS stamp, date or
  environment-dependent option in the binary. Two builds of the same tree
  and VERSION give the same bytes: `make reproducible` builds two copies in
  different directories, with separate build caches, using the same
  recipe as `make release`.
- Module sums are checked (`GOSUMDB=sum.golang.org`, `GOPROXY` set,
  `GONOSUMDB`, `GOPRIVATE`, `GOINSECURE`, `GOEXPERIMENT` cleared).
- No dependency outside the standard library: the SBOM lists the module and
  the Go standard library only. The integration tests use separate modules
  (`integration/testdata/gofunc`), never linked into the binary.

## SBOM

`make sbom` runs syft (`anchore/syft`, pinned by digest, in a container
without network and without its update check) on each binary and writes `<binary>.cdx.json` (CycloneDX JSON). Its random
`serialNumber` and its `timestamp` are removed, so the SBOMs are
reproducible too; they are covered by `SHA256SUMS`, which lists the four
files explicitly. Two `make release` of the same commit give the same
`SHA256SUMS`. The module's version reads `UNKNOWN` in the SBOM: the binary
is built with `-buildvcs=false` (reproducibility) and syft takes the
version from the module information; the release version is in the file
name and in `SHA256SUMS`.

## Signing (not active yet)

`SHA256SUMS` is signed by one of two methods, selected with `SIGN`. None is
active until the maintainers choose; no key is generated or stored in the
repository.

### `SIGN=minisign`: a key held by the maintainer

- One-time: the maintainer creates a key pair offline, **outside the
  repository** (`cd ~/keys && minisign -G -p loch-lambda-runtime.pub -s
  loch-lambda-runtime.key`), keeps the secret key password-protected and
  backed up (`*.key` is in `.gitignore` anyway).
- The public key must reach verifiers through a channel independent of the
  release: Loch.sh Panel embeds it in its own build and passes it with
  `-P`. Copies in the README are a convenience, not a second trust anchor.
- Release: `make release SIGN=minisign MINISIGN_KEY=/home/me/keys/loch-lambda-runtime.key`
  (a plain path: letters, digits, `.`, `_`, `-`, `/`). minisign runs in a
  container pinned by digest, without network, read-only, without
  capabilities; it asks for the password. Output: `SHA256SUMS.minisig`.
- Verify: `minisign -Vm SHA256SUMS -P <public key>`, then
  `sha256sum -c --ignore-missing SHA256SUMS` (macOS:
  `shasum -a 256 -c --ignore-missing SHA256SUMS`).
- Fits offline releases too; the trust root is
  the published public key, rotated by publishing a new one.

### `SIGN=cosign`: keyless, from CI

- Release: in GitHub Actions only (`id-token: write`), when the repository
  variable `SIGN` is `cosign`: the `release-sign` job signs with a
  short-lived certificate from Fulcio and logs it in Rekor. Output:
  `SHA256SUMS.cosign.bundle`. (`make release SIGN=cosign` does the same
  elsewhere, given an OIDC token in `SIGSTORE_ID_TOKEN`.)
- Verify:

  ```sh
  cosign verify-blob SHA256SUMS --bundle SHA256SUMS.cosign.bundle \
    --certificate-identity https://github.com/loch-sh/lambda-runtime/.github/workflows/ci.yml@refs/tags/v1.0.0 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  sha256sum -c SHA256SUMS
  ```

  The exact identity binds the signature to the release workflow and tag.
  To accept every release of a line, use one regexp per line, anchored:
  `--certificate-identity-regexp '^https://github\.com/loch-sh/lambda-runtime/\.github/workflows/ci\.yml@refs/tags/v1\.[0-9]+\.[0-9]+$'`.
- `sha256sum -c SHA256SUMS` checks every file; add `--ignore-missing` only
  when some were not downloaded (one architecture).
- Keyless signing records the repository identity in the public Rekor log.
- No key to keep; the trust root is the CI identity (repository and
  workflow).

On the Loch.sh Panel side, the trust root is the panel build (an embedded
table of checksums and the public key or identity), never a bare
`SHA256SUMS`.

## Build provenance

Every release also carries a GitHub build provenance attestation
(`actions/attest-build-provenance`, Sigstore-signed, whatever `SIGN` is)
for each file listed in `SHA256SUMS`: it binds the binary to the workflow,
the commit and the tag that built it.

```sh
gh attestation verify loch-lambda-runtime-linux-arm64 --repo loch-sh/lambda-runtime
```

## CI

GitHub Actions, `.github/workflows/ci.yml`. Actions are pinned by commit
(the comment gives the version) and kept up to date by Dependabot
(`.github/dependabot.yml`, weekly, grouped); runners are pinned by version
(`ubuntu-26.04`, `ubuntu-26.04-arm`). Default permissions: `contents: read`;
a new push cancels the previous run of a branch or pull request, never of a
tag.

- `test`: gofmt, vet, unit tests with `-race`, coverage, on amd64 and arm64.
- `reproducible`: the release recipe twice, same sha256.
- `integration`: the AWS images under Docker, on amd64 and arm64; on pushes
  and same-repository pull requests, never on forks (it starts containers).
- On `vX.Y.Z[-suffix]` tags, three jobs, each with what it needs only:
  - `release-build` (read): the tag is validated in bash, then by make,
    which never expands it; no Go cache; syft from `anchore/sbom-action`
    (pinned syft version) for the SBOMs; uploads `dist/`;
  - `release-sign` (OIDC token, in the `release` environment: add required
    reviewers): checks `SHA256SUMS`, attests the build provenance, and signs
    `SHA256SUMS` with cosign (`sigstore/cosign-installer`, pinned cosign
    version) when the variable `SIGN` is `cosign`;
  - `release-publish` (write, `release` environment): checks `SHA256SUMS`
    and, when `REQUIRE_SIGNATURE` is 1 (the default), the signature, then
    creates the GitHub release (`softprops/action-gh-release`; a tag with a
    `-suffix` is a prerelease).

`release-build` also refuses a tag whose commit is not on `main`, a `SIGN`
other than `none` or `cosign` (minisign needs the maintainer's key, never in
CI), and `SIGN=none` with `REQUIRE_SIGNATURE=1`: tags fail at once until
the signature is set up. `release-publish` publishes exactly the files of
`SHA256SUMS` (plus the bundle), and verifies the cosign signature against
this workflow's identity for this tag.

**Prerequisites, before the first tag** (repository settings: without
them, anyone with write access could release from any commit):

- a ruleset protecting the `v*` tags (creation by maintainers only);
- the `release` environment, with required reviewers and a deployment
  policy limited to `v*` tags (`release-sign` and `release-publish` both
  use it: two approvals per release);
- the repository variables `SIGN` (`cosign`) and `REQUIRE_SIGNATURE` (`1`);
- a public repository (build provenance attestations need it, or GitHub
  Enterprise).

Dependabot updates the actions after a cooldown of 7 days. The versions
pinned by hand are updated by hand: `go.mod` and `GOTOOLCHAIN` (Makefile),
`syft-version` and `cosign-release` (ci.yml), the tool images (Makefile,
pinned by digest). `download-syft` checks syft against the checksums of
its own release, a weaker anchor than the digest-pinned image `make sbom`
uses locally.
