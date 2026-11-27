#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Validate a component capability declaration.

Cubelet reads each component's `capabilities.json` to decide which capture path a
sandbox may take and whether its guest can freeze a filesystem. An unreadable or
empty declaration does not fail anything loudly at runtime: the reader logs a
warning and falls back to the pre-mechanism behaviour, so a broken artifact would
quietly downgrade every sandbox created from it. Checking it at build time keeps
that out of a release.

The check covers exactly what Cubelet's reader requires, and nothing about
specific capability names: the vocabulary is meant to grow, and a build-time
allowlist would have to be updated in lockstep with each component — the coupling
the declaration exists to remove.

Usage: check-component-capabilities.py <capabilities.json> [expected-component]

`expected-component`, when given, pins the component name the document must
declare (for example `cube-shim`); otherwise any non-empty name is accepted.
Exit status 0 when the file is usable, 1 with a reason on stderr otherwise.
"""

import json
import re
import sys

# Cubelet refuses any other schema rather than guessing at its meaning.
EXPECTED_SCHEMA = 1

# Bounds mirrored from Cubelet's reader, which rejects a declaration that
# exceeds one of them at runtime.
MAX_DOCUMENT_BYTES = 64 * 1024
MAX_ENTRIES = 128
MAX_NAME_LENGTH = 128
VALID_NAME = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
MAX_LEVEL = 0xFFFFFFFF


def fail(message):
    print(f"invalid capability declaration: {message}", file=sys.stderr)
    return 1


def main(argv):
    if len(argv) not in (2, 3):
        return fail("expected a path and an optional component name")
    path = argv[1]
    expected_component = argv[2] if len(argv) == 3 else None

    try:
        with open(path, encoding="utf-8") as handle:
            raw = handle.read()
    except OSError as err:
        return fail(f"cannot read {path}: {err}")

    if not raw.strip():
        return fail(f"{path} is empty")

    # A larger document is refused before parsing.
    if len(raw.encode("utf-8")) > MAX_DOCUMENT_BYTES:
        return fail(f"{path} exceeds {MAX_DOCUMENT_BYTES} bytes")

    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as err:
        return fail(f"{path} is not valid JSON: {err}")

    if not isinstance(doc, dict):
        return fail(f"{path} is not a JSON object")

    # bool is a subclass of int and 1.0 == 1 in Python; the reader's Go int
    # accepts neither.
    schema = doc.get("schema")
    if not isinstance(schema, int) or isinstance(schema, bool):
        return fail(f"{path} has schema {schema!r}, expected {EXPECTED_SCHEMA}")
    if schema != EXPECTED_SCHEMA:
        return fail(f"{path} has schema {schema!r}, expected {EXPECTED_SCHEMA}")

    component = doc.get("component")
    if not isinstance(component, str) or not component:
        return fail(f"{path} declares no component name")
    if expected_component is not None and component != expected_component:
        return fail(
            f'{path} declares component {component!r}, expected "{expected_component}"'
        )

    capabilities = doc.get("capabilities")
    if not isinstance(capabilities, dict) or not capabilities:
        return fail(f"{path} declares no capabilities")
    if len(capabilities) > MAX_ENTRIES:
        return fail(
            f"{path} declares {len(capabilities)} capabilities, limit is {MAX_ENTRIES}"
        )

    for name, level in capabilities.items():
        if not isinstance(name, str) or not name:
            return fail(f"{path} has a non-string capability name: {name!r}")
        if len(name.encode("utf-8")) > MAX_NAME_LENGTH:
            return fail(
                f"{path} has a capability name longer than {MAX_NAME_LENGTH} bytes: {name!r}"
            )
        if not VALID_NAME.match(name):
            return fail(f"{path} has capability name {name!r}, expected a lower-case dotted path")
        # A level decodes into uint32 and must be positive: 0 states a runtime
        # probe, never a shipped capability.
        if not isinstance(level, int) or isinstance(level, bool) or level < 1:
            return fail(
                f"{path} declares {name} at level {level!r}, "
                "expected a positive integer"
            )
        if level > MAX_LEVEL:
            return fail(
                f"{path} declares {name} at level {level!r}, "
                f"expected an integer in [1, {MAX_LEVEL}]"
            )

    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
