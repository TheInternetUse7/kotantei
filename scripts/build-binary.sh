#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 4 ]] || { printf 'Usage: build-binary.sh VERSION REVISION SOURCE_URL OUTPUT_FILE\n' >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
version="$1" revision="$2" source="$3" output="$4"
if [[ "$version" == dev ]]; then
  [[ -z "$revision" && -z "$source" ]] || { printf 'Unstamped development builds must not claim a release revision or source archive.\n' >&2; exit 1; }
else
  bash "$root/scripts/check-release-identity.sh" "$version" "$revision" "$source"
fi
[[ ! -e "$output" ]] || { printf 'Refusing to overwrite an existing binary.\n' >&2; exit 1; }
output="$(realpath -m "$output")"
mkdir -p "$(dirname "$output")"
cd "$root"
export GOTOOLCHAIN=local CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://proxy.golang.org}"
flags="-s -w -X=kotantei/internal/buildinfo.Version=$version -X=kotantei/internal/buildinfo.Revision=$revision -X=kotantei/internal/buildinfo.SourceURL=$source"
go build -trimpath -mod=readonly -ldflags "$flags" -o "$output" ./cmd/kotantei
