#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f scripts/image-tags.sh ]]; then
  printf 'Release image tagging has no stable-latest policy helper.\n' >&2
  exit 1
fi

image=ghcr.io/theinternetuse7/kotantei
check() {
  local version="$1" latest="$2" expected="$3" actual
  actual="$(bash scripts/image-tags.sh "$version" "$image" "$latest")"
  if [[ "$actual" != "$expected" ]]; then
    printf 'Wrong image tags for %s (latest release %s):\n%s\n' "$version" "$latest" "$actual" >&2
    exit 1
  fi
}

check v1.2.3 v1.2.3 "$image:v1.2.3"$'\n'"$image:latest"
check v1.2.2 v1.2.3 "$image:v1.2.2"
check v1.3.0-rc.1 v1.2.3 "$image:v1.3.0-rc.1"
check v1.3.0-rc.1 v1.3.0-rc.1 "$image:v1.3.0-rc.1"
check v1.2.3 '' "$image:v1.2.3"

for version in dev latest v01.2.3 $'v1.2.3\nextra'; do
  if bash scripts/image-tags.sh "$version" "$image" v1.2.3 >/dev/null 2>&1; then
    printf 'Invalid image version was accepted.\n' >&2
    exit 1
  fi
done
for invalid_image in 'ghcr.io/OWNER/Kotantei' 'docker.io/owner/repository' $'ghcr.io/owner/repository\nextra'; do
  if bash scripts/image-tags.sh v1.2.3 "$invalid_image" v1.2.3 >/dev/null 2>&1; then
    printf 'Invalid release image name was accepted.\n' >&2
    exit 1
  fi
done
printf 'Stable latest image-tag policy tests passed.\n'
