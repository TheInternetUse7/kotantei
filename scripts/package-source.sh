#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 4 ]] || { printf 'Usage: package-source.sh TAG REVISION SOURCE_URL OUTPUT_DIRECTORY\n' >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
tag="$1" revision="$2" source="$3"
bash "$root/scripts/check-release-identity.sh" "$tag" "$revision" "$source"
bash "$root/scripts/check-public-config.sh" "$root/compose.yaml" "$root/config.example.jsonc"
output="$(realpath -m "$4")"
archive="$output/kotantei_${tag}_source.tar.gz"
[[ ! -e "$archive" ]] || { printf 'Refusing to overwrite an existing source archive.\n' >&2; exit 1; }
export LC_ALL=C GOTOOLCHAIN=local CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://proxy.golang.org}"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
stage="$work/kotantei-$tag"
mkdir -p "$stage" "$work/dependencies" "$output"
# Explicit public inputs prevent accidental inclusion of secrets, databases, or local work.
for input in cmd internal docs scripts .github Dockerfile compose.yaml .dockerignore .gitignore go.mod go.sum config.example.jsonc LICENSE NOTICE README.md; do
  cp -R "$root/$input" "$stage/$input"
done
printf '{"version":"%s","revision":"%s","source_url":"%s","license":"AGPL-3.0-only","go_version":"%s"}\n' "$tag" "$revision" "$source" "$(go env GOVERSION)" > "$stage/BUILDINFO.json"
cp "$root/go.mod" "$root/go.sum" "$work/dependencies/"
(
  cd "$work/dependencies"
  # Download the entire pinned graph in isolation: never add sums to the original source tree.
  go mod download all
  go mod verify
  bash "$root/scripts/collect-licenses.sh" "$stage/licenses" module-graph
  go list -mod=readonly -m -f '{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.GoMod}}{{"\t"}}{{if .Replace}}replaced{{else}}original{{end}}{{end}}' all > "$work/modules"
)
proxy="$stage/dependency-source"
cache="$(realpath -m "$(go env GOMODCACHE)/cache/download")"
mkdir -p "$proxy"
printf 'module\tversion\n' > "$proxy/MODULES.tsv"
while IFS=$'\t' read -r module version modfile origin; do
  [[ -n "$module" ]] || continue
  modfile="$(realpath -m "$modfile")"
  [[ "$origin" == original && "$modfile" == "$cache/"* && "$modfile" == *.mod ]] || { printf 'A dependency cannot be packaged from the verified module cache.\n' >&2; exit 1; }
  relative="${modfile#"$cache"/}"
  destination="$proxy/${relative%.mod}"
  mkdir -p "$(dirname "$destination")"
  for extension in mod info zip; do cp "${modfile%.mod}.$extension" "$destination.$extension"; done
  printf '%s\n' "$version" >> "$(dirname "$destination")/list"
  printf '%s\t%s\n' "$module" "$version" >> "$proxy/MODULES.tsv"
done < "$work/modules"
(
  cd "$proxy"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
)
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$work" -cf - "kotantei-$tag" | gzip -n > "$archive"
