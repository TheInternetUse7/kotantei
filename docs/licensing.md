# Licensing and corresponding source

Kotantei is **AGPL-3.0-only**, copyright 2026 theinternetuser. See [LICENSE](../LICENSE) for the full license and [NOTICE](../NOTICE) for copyright and warranty notices. Third-party components retain their own licenses. Their full discovered notices are bundled under `licenses/` in release archives and `/usr/share/kotantei/licenses` in images.

## Distributing binaries or images

Preserve copyright/license/warranty notices and comply with the full applicable licenses. When conveying non-source forms, supply the machine-readable **Corresponding Source** by an allowed method. For network downloads, AGPL section 6(d) permits equivalent source access with clear directions alongside the binary; you remain responsible for source availability even when a different server hosts it.

The release workflow publishes a matching source asset before publishing GHCR images. That asset includes Kotantei source and build/install/deployment scripts, module versions/checksums, full pinned dependency-source copies, and discovered third-party notices. GitHub's automatic repository archive alone does not include those dependency-source copies. Release notes, compiled identity, and the image's `io.kotantei.corresponding-source` label point to the dedicated source asset.

The compiler and general-purpose CI tools are not bundled. Container-base components retain their own terms and source/notice obligations; Kotantei's AGPL label does not relicense the Debian filesystem or replace those obligations. Preserve accompanying base notices and consult their upstream source and license information when redistributing them.

## Running modified versions over a network

AGPL section 13 requires a modified network-interactive version to prominently offer all remote users its matching Corresponding Source, free of charge through a standard copying mechanism. Publish **the version actually running**, including your changes and required build scripts; a link to an unrelated upstream release or repository homepage is not enough.

`/about` provides the copyright, warranty statement, license link, version, and corresponding-source title link. Official releases compile their source-asset URL. For modified deployments, set `project.source_url` in `config.jsonc` to a public HTTPS download without credentials. The override takes precedence. Provide a matching download when distributing a modified build.

`kotantei version` reports the compiled version/revision/source/license without reading configuration or credentials. It does not report an operator override; use `/about` to verify the running interface's offer. Keep source links accessible without a login, retain full notices, prominently identify your modifications and relevant dates as required, and do not impose extra restrictions on recipients' licensed rights.

Publish source code and build material, **not** Discord/MAL secrets, linked-user databases, backups, or private account data. The bot requires no provider user OAuth login and must not obtain private account information. AGPL source obligations do not require exposing operational credentials or users' records.

This page is practical project guidance, not legal advice or a replacement for the license text. There is no warranty; review the license itself for the precise conditions, exceptions, and any installation-information requirements that apply to your distribution.
