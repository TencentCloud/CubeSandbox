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
import sys

# Cubelet refuses any other schema rather than guessing at its meaning.
EXPECTED_SCHEMA = 1


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

    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as err:
        return fail(f"{path} is not valid JSON: {err}")

    if not isinstance(doc, dict):
        return fail(f"{path} is not a JSON object")

    if doc.get("schema") != EXPECTED_SCHEMA:
        return fail(
            f"{path} has schema {doc.get('schema')!r}, expected {EXPECTED_SCHEMA}"
        )

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

    # Every level must be a positive integer. Level 0 is a real statement about a
    # runtime probe, but no build should make it about a capability it ships.
    for name, level in capabilities.items():
        if not isinstance(name, str) or not name:
            return fail(f"{path} has a non-string capability name: {name!r}")
        # bool is a subclass of int; `true` here would be a malformed document.
        if not isinstance(level, int) or isinstance(level, bool) or level < 1:
            return fail(
                f"{path} declares {name} at level {level!r}, "
                "expected a positive integer"
            )

    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
