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
- Fits any remote (GitHub or GitLab) and offline releases; the trust root is
  the published public key, rotated by publishing a new one.

### `SIGN=cosign`: keyless, from CI

- Release: in CI only, with an OIDC identity token (GitHub Actions
  `id-token: write`, or GitLab `id_tokens` with `aud: sigstore`) exported as
  `SIGSTORE_ID_TOKEN`. `make release SIGN=cosign` signs with a short-lived
  certificate from Fulcio and logs it in Rekor. Output:
  `SHA256SUMS.cosign.bundle`.
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
  GitLab: identity `https://gitlab.com/<group>/lambda-runtime//.gitlab-ci.yml@refs/tags/v1.0.0`,
  issuer `https://gitlab.com`, or the URL of a self-hosted GitLab instance.
- `sha256sum -c SHA256SUMS` checks every file; add `--ignore-missing` only
  when some were not downloaded (one architecture).
- Keyless signing records the repository identity in the public Rekor log.
- No key to keep; the trust root is the CI identity (repository and
  workflow), so it depends on the chosen remote.

On the Loch.sh Panel side, the trust root is the panel build (an embedded
table of checksums and the public key or identity), never a bare
`SHA256SUMS`.

## CI

Drafts, not active until a remote is chosen:

- GitHub Actions: `.github/workflows/ci.yml` (runners pinned to
  `ubuntu-24.04`): vet, unit tests with `-race`, coverage, reproducibility;
  integration (Docker) on pushes and same-repository pull requests, never
  on forks; on `vX.Y.Z` tags, three jobs, each with one permission:
  `release-build` (read; the tag is validated in bash, then by make, which
  never expands it; no Go cache), `release-sign` (OIDC token only, in the
  `release` environment: add required reviewers; cosign keyless when the
  variable `SIGN` is `cosign`), `release-publish` (write; checks
  `SHA256SUMS`, and the signature when `REQUIRE_SIGNATURE` is 1, the
  default).
- GitLab CI: `.gitlab-ci.yml`: same tests; integration on a shell runner
  with Docker (tag `docker-host`), for protected refs only: Docker access
  is root on that host, so the runner must be dedicated or ephemeral and
  locked to protected refs, never run merge request code. On tags (assumed
  protected, with `SIGN` and `REQUIRE_SIGNATURE` as protected variables),
  build, SBOM and signature are separate jobs running the pinned tool
  images directly (their debug/dev variants, which have a shell;
  Docker-in-Docker could not see the job's files); the signature job checks
  `SHA256SUMS` first. Release artifacts are kept (`expire_in: never`);
  creating a GitLab Release is left to the maintainers.
