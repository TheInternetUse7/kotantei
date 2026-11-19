#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

invalid() {
  printf 'Release tag must be vMAJOR.MINOR.PATCH with an optional SemVer prerelease, at most 128 bytes; build metadata is not supported.\n' >&2
  exit 1
}

[[ "$#" == 1 ]] || invalid
tag="$1"
[[ "${#tag}" -le 128 ]] || invalid
pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$'
[[ "$tag" =~ $pattern ]] || invalid
if [[ "$tag" == *-* ]]; then
  IFS=. read -r -a identifiers <<< "${tag#*-}"
  for identifier in "${identifiers[@]}"; do
    if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" == 0?* ]]; then invalid; fi
  done
fi
