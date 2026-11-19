#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
[[ "$#" == 3 ]] || { printf 'Expected release tag, full commit revision, and matching HTTPS source URL.\n' >&2; exit 1; }
bash "$(dirname "$0")/check-release-tag.sh" "$1"
[[ "$2" =~ ^[0-9a-f]{40}$ ]] || { printf 'Release revision must be a full lowercase 40-character commit ID supplied by CI.\n' >&2; exit 1; }
# Keep linker values free of whitespace, quotes, credentials, and malformed escapes.
pattern='^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?(/([A-Za-z0-9._~:/?#@!$&()*+,;=-]|%[0-9A-Fa-f]{2})*)?$'
[[ "${#3}" -le 2048 && "$3" =~ $pattern ]] || { printf 'Release source URL must be a bounded, safely encoded HTTPS URL without credentials.\n' >&2; exit 1; }
