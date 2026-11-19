#!/usr/bin/env bash
# Run from the Go module root. Copy full notices, not summaries or module-cache paths.
set -euo pipefail
export LC_ALL=C GOTOOLCHAIN=local CGO_ENABLED=0
[[ "$#" -ge 1 && "$#" -le 2 ]] || { printf 'Usage: collect-licenses.sh OUTPUT_DIRECTORY [runtime|module-graph]\n' >&2; exit 1; }
scope="${2:-runtime}"
[[ "$scope" == runtime || "$scope" == module-graph ]] || { printf 'Unknown license inventory scope.\n' >&2; exit 1; }
output="$(realpath -m "$1")"
[[ ! -e "$output" ]] || { printf 'Refusing to overwrite an existing license directory.\n' >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
template='{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{"\t"}}{{if .Replace}}replaced{{else}}original{{end}}{{end}}'
if [[ "$scope" == runtime ]]; then
  go list -mod=readonly -deps -f "{{with .Module}}$template{{end}}" ./cmd/kotantei > "$work/modules"
  scope="runtime:$(go env GOOS)/$(go env GOARCH)"
else
  go list -mod=readonly -m -f "$template" all > "$work/modules"
fi
sort -u "$work/modules" | sed '/^$/d' > "$work/sorted"
mkdir -p "$output/go" "$output/modules"
printf 'scope\tmodule\tversion\n' > "$output/INDEX.tsv"

copy_notices() {
  local directory="$1" destination="$2" notice relative count=0
  while IFS= read -r -d '' notice; do
    relative="${notice#"$directory"/}"
    mkdir -p "$destination/$(dirname "$relative")"
    cp "$notice" "$destination/$relative"
    count=$((count + 1))
  done < <(find "$directory" -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'COPYRIGHT*' -o -iname 'AUTHORS*' -o -iname 'PATENTS*' \) -print0)
  [[ "$count" -gt 0 ]] || { printf 'A dependency has no discoverable notice files; review its license before release.\n' >&2; exit 1; }
}

goroot="$(go env GOROOT)"
cp "$goroot/LICENSE" "$output/go/LICENSE"
if [[ -f "$goroot/PATENTS" ]]; then cp "$goroot/PATENTS" "$output/go/PATENTS"; fi
copy_notices "$goroot/src" "$output/go/src"
printf '%s\tGo\t%s\n' "$scope" "$(go env GOVERSION)" >> "$output/INDEX.tsv"
while IFS=$'\t' read -r module version directory origin; do
  [[ -n "$version" && -d "$directory" && "$origin" == original ]] || { printf 'Missing or replaced module source prevents a complete license inventory.\n' >&2; exit 1; }
  copy_notices "$directory" "$output/modules/$module@$version"
  printf '%s\t%s\t%s\n' "$scope" "$module" "$version" >> "$output/INDEX.tsv"
done < "$work/sorted"
