#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 2 ]] || { printf 'Usage: check-public-config.sh COMPOSE_FILE EXAMPLE_CONFIG\n' >&2; exit 1; }
python3 - "$1" "$2" <<'PY'
import json
import pathlib
import re
import sys

def string_end(text, start):
    index = start + 1
    while index < len(text):
        if text[index] == "\\":
            index += 2
        elif text[index] == '"':
            return index
        else:
            index += 1
    return len(text)

def normalize_jsonc(source):
    text = list(source)
    index = 0
    while index < len(text):
        if text[index] == '"':
            index = string_end(text, index) + 1
            continue
        if text[index:index + 2] == ["/", "/"]:
            end = index + 2
            while end < len(text) and text[end] not in "\r\n":
                end += 1
        elif text[index:index + 2] == ["/", "*"]:
            closing = source.find("*/", index + 2)
            if closing < 0:
                raise ValueError("Unterminated comment")
            end = closing + 2
        else:
            index += 1
            continue
        for position in range(index, end):
            if text[position] not in "\r\n":
                text[position] = " "
        index = end
    index = 0
    while index < len(text):
        if text[index] == '"':
            index = string_end(text, index) + 1
            continue
        if text[index] == ",":
            following = index + 1
            while following < len(text) and text[following] in " \t\r\n":
                following += 1
            previous = index - 1
            while previous >= 0 and text[previous] in " \t\r\n":
                previous -= 1
            if following < len(text) and text[following] in "}]" and previous >= 0 and text[previous] not in "[{,:":
                text[index] = " "
        index += 1
    return "".join(text)

def invalid(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)

try:
    compose = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
    example = pathlib.Path(sys.argv[2]).read_text(encoding="utf-8")
    if len(example.encode("utf-8")) > 1024 * 1024:
        invalid("Public example configuration exceeds its size limit.")
    config = json.loads(normalize_jsonc(example))
except (OSError, ValueError):
    invalid("Cannot validate public configuration templates.")

if "${" in compose:
    invalid("Public Compose settings must not depend on environment interpolation.")
if re.search(r"\bKOTANTEI_(DISCORD_TOKEN|MAL_CLIENT_ID)\b", compose):
    invalid("Compose must mount configuration rather than duplicate credentials.")
if not isinstance(config, dict):
    invalid("The public example configuration must be an object.")
for group, key in (("discord", "token"), ("mal", "client_id")):
    section = config.get(group)
    if not isinstance(section, dict) or section.get(key) != "":
        invalid("Public example credentials must remain blank.")
PY
