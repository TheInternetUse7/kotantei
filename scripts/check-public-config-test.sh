#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ -f scripts/check-public-config.sh ]] || { printf 'Source releases do not guard public configuration credentials.\n' >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
printf 'services:\n  kotantei:\n    command: ["--config", "/run/secrets/kotantei_config.jsonc", "run"]\n    secrets: [kotantei_config]\nsecrets:\n  kotantei_config:\n    file: ./config.jsonc\n' > "$work/compose.yaml"
cat > "$work/config.jsonc" <<'JSONC'
// Credentials belong in the private deployment copy.
{
  "discord": {"token": /* Discord bot token */ "",},
  "mal": {"client_id": "",},
}
JSONC
bash scripts/check-public-config.sh "$work/compose.yaml" "$work/config.jsonc"

printf 'services:\n  kotantei:\n    environment:\n      KOTANTEI_DISCORD_TOKEN: "DO-NOT-PRINT-CREDENTIAL"\n' > "$work/private.yaml"
if bash scripts/check-public-config.sh "$work/private.yaml" "$work/config.jsonc" > "$work/output" 2>&1; then
  printf 'Source release accepted a configured Compose token.\n' >&2
  exit 1
fi
if grep -q DO-NOT-PRINT "$work/output"; then
  printf 'Public-config validation exposed credentials.\n' >&2
  exit 1
fi
for private_config in \
  '{"discord":{"token":""},"mal":{"client_id":"DO-NOT-PRINT-CREDENTIAL"}}' \
  '{"discord":{"token":"DO-NOT-PRINT-CREDENTIAL"},"mal":{"client_id":""}}' \
  '{"discord":{"token":"","to\u006ben":"DO-NOT-PRINT-CREDENTIAL"},"mal":{"client_id":""}}' \
  '/* DO-NOT-PRINT-CREDENTIAL unterminated'; do
  printf '%s\n' "$private_config" > "$work/private.jsonc"
  if bash scripts/check-public-config.sh "$work/compose.yaml" "$work/private.jsonc" > "$work/output" 2>&1; then
    printf 'Source release accepted configured or malformed example credentials.\n' >&2
    exit 1
  fi
  if grep -q DO-NOT-PRINT "$work/output"; then
    printf 'Public-config validation exposed credentials.\n' >&2
    exit 1
  fi
done
printf 'Public configuration credential-guard tests passed.\n'
