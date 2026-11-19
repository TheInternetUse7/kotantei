#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
revision=0123456789012345678901234567890123456789
bash scripts/check-release-identity.sh v1.2.3 "$revision" https://example.com/source.tar.gz
bash scripts/check-release-identity.sh v1.2.3-rc.1 "$revision" 'https://example.com/source%20archive.tar.gz?download=1'
for source in 'http://example.com/DO-NOT-PRINT' 'https://user:DO-NOT-PRINT@example.com/source' 'https://:443/DO-NOT-PRINT' 'https://example.com/invalid%escape' $'https://example.com/source\nDO-NOT-PRINT' "https://example.com/$(printf '%*s' 2048 '' | tr ' ' x)"; do
  if bash scripts/check-release-identity.sh v1.2.3 "$revision" "$source" >/dev/null 2>&1; then
    printf 'Unsafe release source URL was accepted.\n' >&2
    exit 1
  fi
done
for revision in '' short 012345678901234567890123456789012345678g; do
  if bash scripts/check-release-identity.sh v1.2.3 "$revision" https://example.com/source.tar.gz >/dev/null 2>&1; then
    printf 'Invalid release revision was accepted.\n' >&2
    exit 1
  fi
done
if bash scripts/check-release-identity.sh >/dev/null 2>&1 || bash scripts/check-release-identity.sh v1.2.3 "$revision" https://example.com/source.tar.gz extra >/dev/null 2>&1; then
  printf 'Release identity validator accepted an invalid argument count.\n' >&2
  exit 1
fi
printf 'Release identity validation tests passed.\n'
