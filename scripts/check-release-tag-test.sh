#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
for tag in v0.1.0 v1.2.3 v1.2.3-rc.1 v1.2.3-0 v1.2.3-alpha-01 "v1.2.3-$(printf '%*s' 121 '' | tr ' ' x)"; do
  if ! bash scripts/check-release-tag.sh "$tag"; then
    printf 'Valid release tag was rejected: %s\n' "$tag" >&2
    exit 1
  fi
done
for tag in dev latest 1.2.3 v01.2.3 v1.02.3 v1.2.03 v1.2.3+build v1.2.3-01 v1.2.3-rc..1 v1.2.3-rc_1 'v1.2.3/other' $'v1.2.3\nextra' $'v1.2.3\n' "v1.2.3-$(printf '%*s' 122 '' | tr ' ' x)"; do
  if bash scripts/check-release-tag.sh "$tag" >/dev/null 2>&1; then
    printf 'Invalid release tag was accepted: %s\n' "$tag" >&2
    exit 1
  fi
done
for arguments in 0 2; do
  args=()
  if [[ "$arguments" == 2 ]]; then args=(v1.2.3 extra); fi
  if bash scripts/check-release-tag.sh "${args[@]}" >/dev/null 2>&1; then
    printf 'Release tag validator accepted an invalid argument count.\n' >&2
    exit 1
  fi
done
printf 'Release tag validation tests passed.\n'
