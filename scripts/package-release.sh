#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 4 ]] || { printf 'Usage: package-release.sh TAG REVISION SOURCE_URL NEW_OUTPUT_DIRECTORY\n' >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
tag="$1" revision="$2" source="$3"
bash "$root/scripts/check-release-identity.sh" "$tag" "$revision" "$source"
output="$(realpath -m "$4")"
[[ ! -e "$output" ]] || { printf 'Refusing to overwrite an existing release directory.\n' >&2; exit 1; }
export LC_ALL=C GOTOOLCHAIN=local CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://proxy.golang.org}"
cd "$root"
go mod download
go mod verify
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$output"
bash "$root/scripts/package-source.sh" "$tag" "$revision" "$source" "$output"
for target in linux/amd64 linux/arm64 windows/amd64; do
  export GOOS="${target%/*}" GOARCH="${target#*/}"
  name="kotantei-$tag-$GOOS-$GOARCH"
  stage="$work/$name"
  mkdir "$stage"
  binary=kotantei
  if [[ "$GOOS" == windows ]]; then binary=kotantei.exe; fi
  bash "$root/scripts/build-binary.sh" "$tag" "$revision" "$source" "$stage/$binary"
  bash "$root/scripts/collect-licenses.sh" "$stage/licenses"
  for input in LICENSE NOTICE README.md config.example.jsonc docs; do cp -R "$root/$input" "$stage/$input"; done
  tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$work" -cf - "$name" | gzip -n > "$output/kotantei_${tag}_${GOOS}_${GOARCH}.tar.gz"
done
(cd "$output" && sha256sum ./*.tar.gz > SHA256SUMS)
