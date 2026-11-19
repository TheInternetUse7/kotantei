#!/usr/bin/env bash
# Exercise real Go builds, archives, notices, and an offline source rebuild.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -e THIRD_PARTY_NOTICES.md || ! -f config.example.jsonc ]]; then
  printf 'Public release inputs must use the JSONC template without the hand-written notice index.\n' >&2
  exit 1
fi
export GOTOOLCHAIN=local CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://proxy.golang.org}"
cache="$(realpath -m "$(go env GOMODCACHE)")"
mkdir -p "$cache"
# Go cleans cache paths in module metadata; source packaging must also handle a non-canonical configured path.
export GOMODCACHE="$cache/../$(basename "$cache")"
work="$(mktemp -d)"
# Go makes extracted module directories read-only; cleanup must work without root.
trap 'chmod -R u+w -- "$work"; rm -rf -- "$work"' EXIT
tag=v0.0.0-test.1
revision=0123456789012345678901234567890123456789
source="https://github.com/theinternetuse7/Kotantei/releases/download/${tag}/kotantei_${tag}_source.tar.gz"
cp go.mod "$work/original-go.mod"
cp go.sum "$work/original-go.sum"

bash scripts/package-release.sh "$tag" "$revision" "$source" "$work/assets"
cmp go.mod "$work/original-go.mod"
cmp go.sum "$work/original-go.sum"
[[ "$(find "$work/assets" -maxdepth 1 -name '*.tar.gz' | wc -l)" == 4 ]]
(cd "$work/assets" && sha256sum --check SHA256SUMS)

mkdir "$work/native" "$work/source"
tar -xzf "$work/assets/kotantei_${tag}_linux_amd64.tar.gz" -C "$work/native"
native="$work/native/kotantei-${tag}-linux-amd64"
KOTANTEI_SOURCE_URL='http://DO-NOT-PRINT' "$native/kotantei" --config "$work/missing.json" version > "$work/version.json"
python3 - "$work/version.json" "$tag" "$revision" "$source" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as file:
    report = json.load(file)
assert report["version"] == sys.argv[2], report
assert report["revision"] == sys.argv[3], report
assert report["source_url"] == sys.argv[4], report
assert report["license"] == "AGPL-3.0-only", report
PY
go version -m "$native/kotantei" > "$work/binary-metadata.txt"
grep -q 'CGO_ENABLED=0' "$work/binary-metadata.txt"
grep -q -- '-trimpath=true' "$work/binary-metadata.txt"
# Go may include VCS metadata from a checkout. Application identity is checked above.
for module in modernc.org/sqlite modernc.org/libc modernc.org/memory; do
  directory="$(go list -mod=readonly -m -f '{{.Dir}}' "$module")"
  version="$(go list -mod=readonly -m -f '{{.Version}}' "$module")"
  for notice in "$directory"/LICENSE*; do
    cmp "$notice" "$native/licenses/modules/$module@$version/$(basename "$notice")"
  done
done
cmp "$(go env GOROOT)/LICENSE" "$native/licenses/go/LICENSE"

tar -xzf "$work/assets/kotantei_${tag}_source.tar.gz" -C "$work/source"
root="$work/source/kotantei-${tag}"
cmp go.mod "$root/go.mod"
cmp go.sum "$root/go.sum"
cmp LICENSE "$root/LICENSE"
cmp NOTICE "$root/NOTICE"
[[ -f "$root/scripts/package-release.sh" && -f "$root/internal/buildinfo/buildinfo.go" && -f "$root/dependency-source/MODULES.tsv" ]]
python3 - "$root" <<'PY'
import pathlib, sys
allowed = {"cmd", "internal", "docs", "scripts", ".github", "Dockerfile", "compose.yaml", ".dockerignore", ".gitignore", "go.mod", "go.sum", "config.example.jsonc", "LICENSE", "NOTICE", "README.md", "BUILDINFO.json", "dependency-source", "licenses"}
assert {p.name for p in pathlib.Path(sys.argv[1]).iterdir()} <= allowed
assert not (pathlib.Path(sys.argv[1]) / ".env.example").exists()
assert not (pathlib.Path(sys.argv[1]) / "THIRD_PARTY_NOTICES.md").exists()
assert (pathlib.Path(sys.argv[1]) / "config.example.jsonc").is_file()
PY
(
  cd "$root"
  export GOPROXY="file://$root/dependency-source" GOSUMDB=off GOMODCACHE="$work/offline-modules"
  GOOS=linux GOARCH=amd64 bash scripts/build-binary.sh "$tag" "$revision" "$source" "$work/rebuilt"
)
"$work/rebuilt" version > "$work/rebuilt-version.json"
cmp "$work/version.json" "$work/rebuilt-version.json"

mkdir "$work/existing"
printf 'preserve\n' > "$work/existing/marker"
if bash scripts/package-release.sh "$tag" "$revision" "$source" "$work/existing" >/dev/null 2>&1; then
  printf 'Packaging overwrote an existing output directory.\n' >&2
  exit 1
fi
[[ "$(cat "$work/existing/marker")" == preserve ]]
if bash scripts/package-release.sh invalid "$revision" "$source" "$work/invalid" >/dev/null 2>&1 || [[ -e "$work/invalid" ]]; then
  printf 'Invalid release identity created output.\n' >&2
  exit 1
fi
printf 'Release packaging, identity, full-notice, and offline rebuild tests passed.\n'
