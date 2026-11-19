# Versioned releases

[`.github/workflows/release.yaml`](../.github/workflows/release.yaml) publishes versioned releases from [theinternetuse7/Kotantei](https://github.com/theinternetuse7/Kotantei) and container images at `ghcr.io/theinternetuse7/kotantei`.

Release tooling requires Go 1.27.4, Bash, Git, GNU tar, gzip, and Python 3. The workflow supplies these tools.

## Tag and identity contract

Release tags are `vMAJOR.MINOR.PATCH`, optionally followed by a SemVer prerelease such as `-rc.1`. Numeric identifiers cannot have leading zeros. Tags must fit GHCR's 128-byte tag limit; build metadata (`+build`) and arbitrary branch names are not release versions. Prerelease tags create GitHub prereleases. Every image receives its exact version tag. The latest stable GitHub release also receives the `latest` image tag; older maintenance releases and prereleases cannot replace it. Image publication is serialized per repository.

The workflow derives version from `github.ref_name`, full revision from `github.sha`, and source URL from this repository's release-asset URL. These values are validated and passed to:

- Go linker variables `kotantei/internal/buildinfo.Version`, `Revision`, and `SourceURL`;
- Docker build arguments and OCI version/revision/source/license metadata;
- the source archive's `BUILDINFO.json`.

Builds use `-trimpath`, disabled CGO, and Go 1.27.4. Go can additionally record VCS metadata when building a checkout; application release identity is supplied explicitly. Unstamped builds report `dev`. `kotantei version` reports compiled identity, and Discord `/about` displays the version/license and matching-source title link. An operator source override affects `/about`, not the compiled identity shown by `version`.

## Workflow and assets

1. Validate the tag and public source availability, check that public configuration credentials are blank, verify modules/formatting, run Go tests/vet and Linux race tests, and exercise release helpers plus an offline source rebuild.
2. Package binaries for **Linux amd64**, **Linux arm64**, and **Windows amd64**. Each archive includes project licensing, configuration/docs, and complete discovered notices for that target's command-build dependencies and Go.
3. Package the matching public application source, build/deployment scripts, and verified full-module `.zip`, `.mod`, and `.info` copies for the entire dependency graph. The allowlist excludes operational configuration, databases, and unrelated local files. Full-graph notices are explicitly distinguished from runtime inventories.
4. Upload `kotantei_TAG_OS_ARCH.tar.gz`, `kotantei_TAG_source.tar.gz`, and `SHA256SUMS` to the existing tag's GitHub release. Publication rechecks the tag's commit through GitHub's API and uses `--verify-tag`: it does not create or move a tag, or overwrite an existing release. Both checkouts are pinned to the original event revision.
5. After the source release succeeds, cross-build and publish the versioned GHCR image for Linux amd64/arm64, plus `latest` when appropriate. Image provenance is enabled.

Action revisions and the Go/Distroless base-image manifests are pinned. Review/update their versions and digests deliberately when patching dependencies. Archives normalize timestamps, ordering, and ownership; this is not a promise that future toolchain/platform changes yield identical binary bytes.

## Repository setup and publishing

The repository must be public for the generated matching-source asset to be downloadable without authentication. Enable GitHub Actions and allow the workflow token to write releases and packages. No long-lived publishing token is configured: jobs receive only the necessary `contents` or `packages` permissions, and checkout does not persist credentials.

An authorized maintainer publishes an appropriately protected version tag pointing to reviewed source. The workflow handles packaging/publication; no local release/tag command is required by these scripts. Do not retarget a published version tag. Protect release tags and ensure untrusted contributors cannot cause tagged code to run with publishing permissions.

GHCR's package name follows the repository slug, lowercased. A package can initially be private even when the source repository is public; set its visibility and repository access deliberately in GitHub package settings. Check both release and image jobs before deploying, and preserve the public matching-source archive for as long as required by the licenses. Prefer a recorded manifest digest for deployment immutability.

If publication fails after the source release succeeds, leave that source available. Retry the failed image job against the same immutable tag rather than deleting source or moving a tag. Re-running a successful release-publication job intentionally fails instead of overwriting an existing release. Do not claim a release/image exists merely because these configuration files are present.

## Source rebuild without a module network

Verify `SHA256SUMS`, extract the **source asset**, and enter its `kotantei-TAG` directory. Use Go 1.27.4. The supplied `dependency-source` tree is a local Go module proxy:

```sh
export GOTOOLCHAIN=local
export GOPROXY="file://$PWD/dependency-source"
export GOSUMDB=off
export GOMODCACHE="$PWD/module-cache"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -mod=readonly -trimpath -o kotantei ./cmd/kotantei
```

`GOSUMDB=off` is for this offline rebuild only; existing `go.sum` checks still apply. Packaging originally verifies module downloads, and the source asset contains an additional `dependency-source/SHA256SUMS`. The command above creates an unstamped `dev` executable. To reproduce release identity, take the tag, full revision, and source URL from `BUILDINFO.json` and pass them to `bash scripts/build-binary.sh TAG REVISION SOURCE_URL OUTPUT_FILE` with the same target environment. No Git discovery is needed.

Run release helper regressions with `bash scripts/check-release-tag-test.sh`, `bash scripts/check-release-identity-test.sh`, `bash scripts/image-tags-test.sh`, `bash scripts/check-public-config-test.sh`, and `bash scripts/release-test.sh`. The packaging test builds archives and performs an offline source rebuild. `package-release.sh TAG REVISION SOURCE_URL NEW_OUTPUT_DIRECTORY` refuses an existing output directory.
