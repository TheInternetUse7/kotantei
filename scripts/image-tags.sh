#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
[[ "$#" == 3 ]] || { printf 'Usage: image-tags.sh VERSION IMAGE LATEST_RELEASE\n' >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
version="$1" image="$2" latest="$3"
bash "$root/scripts/check-release-tag.sh" "$version"
if [[ -n "$latest" ]]; then bash "$root/scripts/check-release-tag.sh" "$latest"; fi
[[ "$image" =~ ^ghcr\.io/[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9._-]*$ ]] || { printf 'Release image must be a lowercase GHCR owner/repository name.\n' >&2; exit 1; }

printf '%s:%s\n' "$image" "$version"
# An older maintenance release or a prerelease must never replace latest.
if [[ "$version" != *-* && "$version" == "$latest" ]]; then
  printf '%s:latest\n' "$image"
fi
