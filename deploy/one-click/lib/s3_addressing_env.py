#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Bridge shell assignment rows containing an S3 addressing setting to the KV merger."""

import re
import sys
from pathlib import Path

STYLES = {"CUBE_S3_ADDRESSING_STYLE", "CUBE_OPS_S3_ADDRESSING_STYLE"}
# Accept scalar assignments only. Keep the original quoting for the downstream
# shell. A comment, quoted space or escaped space does not start another key.
ASSIGNMENT = re.compile(
    r"""([A-Za-z_][A-Za-z_0-9]*)=((?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'"\\;&|<>()`])*)(?=\s|$)"""
)


def style_assignments(line):
    if "S3_ADDRESSING_STYLE=" not in line:
        return []
    text = re.sub(r"^export[ \t]+", "", line.strip())
    pairs = []
    while text and not text.startswith("#"):
        match = ASSIGNMENT.match(text)
        if not match:
            return []
        pairs.append(match.groups())
        text = text[match.end():].lstrip()
    return pairs if any(key in STYLES for key, _ in pairs) else []


# Match literal values. Inside double quotes, $ and backticks can still
# expand despite an apostrophe elsewhere inside the string.
LITERAL_VALUE = re.compile(r"""(?:'[^']*'|"(?:\\.|[^"\\$`])*"|\\.|[^'"\\$`])*""")
KEY_VALUE = re.compile(r"^([A-Za-z_][A-Za-z_0-9]*)=")


def stored_assignments(lines, template):
    normalized = []
    derived_extras = set()
    for line in lines:
        pairs = style_assignments(line)
        if not pairs:
            normalized.append(line)
            continue
        for key, raw in pairs:
            normalized.append(f"{key}={raw}")
            if key not in template:
                derived_extras.add(key)

    # The KV merger uses the first position and the last value. Keep only
    # the last occurrence of extras from the converted rows so references
    # still follow their dependencies in the old runtime.
    output = []
    seen_extras = set()
    for line in reversed(normalized):
        match = KEY_VALUE.match(line)
        if match and match[1] in derived_extras:
            if match[1] in seen_extras:
                continue
            seen_extras.add(match[1])
        output.append(line)
    return reversed(output)


def preserve_merge_order(old_lines, template, merged, keys_file):
    # The merger puts template values before custom variables. Delay an old
    # expression from a compound style row until its original custom inputs
    # have been assigned. Explicit new values already selected by the merger
    # remain in their normal positions.
    old_values = {}
    deferred = {
        key: f"{key}={raw}"
        for line in old_lines
        for key, raw in style_assignments(line)
        if key in template and not LITERAL_VALUE.fullmatch(raw)
    }
    normalized = list(stored_assignments(old_lines, template))
    for line in normalized:
        match = KEY_VALUE.match(line)
        if match:
            old_values[match[1]] = line
    merged_lines = Path(merged).read_text(encoding="utf-8").splitlines()
    merged_values = {}
    for line in merged_lines:
        match = KEY_VALUE.match(line)
        if match:
            merged_values[match[1]] = line
    move = {key: value for key, value in deferred.items()
            if merged_values.get(key) == value == old_values.get(key)}
    keys_file.write_text("".join(f"{key}\n" for key in sorted(move)), encoding="utf-8")
    if not move:
        return

    by_anchor = {}
    anchor = None
    for line in reversed(normalized):
        match = KEY_VALUE.match(line)
        if not match:
            continue
        key = match[1]
        if line == move.get(key):
            by_anchor.setdefault(anchor, []).append(move.pop(key))
        elif key not in template and key in merged_values:
            anchor = key

    moved_rows = {line for group in by_anchor.values() for line in group}
    output = []
    for line in merged_lines:
        if line in moved_rows:
            continue
        match = KEY_VALUE.match(line)
        if match:
            output.extend(reversed(by_anchor.get(match[1], ())))
        output.append(line)
    output.extend(reversed(by_anchor.get(None, ())))
    Path(merged).write_text("\n".join(output) + "\n", encoding="utf-8")


def main():
    mode, source, *paths = sys.argv[1:]
    lines = Path(source).read_text(encoding="utf-8").splitlines() if source else []
    if mode == "keys":
        # Bash has already loaded the input. Only ask for names here, never
        # evaluate an assignment a second time or inspect unrelated environment.
        keys = set(STYLES)
        for line in lines:
            keys.update(key for key, _ in style_assignments(line))
        for key in sorted(keys):
            print(key)
        return
    template_lines = Path(paths[0]).read_text(encoding="utf-8").splitlines()
    template_matches = (KEY_VALUE.match(line) for line in template_lines)
    template = {match[1] for match in template_matches if match}
    if mode == "stored":
        for line in stored_assignments(lines, template):
            print(line)
    elif mode == "order":
        preserve_merge_order(lines, template, paths[1], Path(paths[2]))
    else:
        raise SystemExit("expected keys, stored, or order")


if __name__ == "__main__":
    main()
