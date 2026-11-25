#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Validate a CubeShim capability declaration: exit 0 when usable, 1 otherwise.

Usage: check-shim-capabilities.py <capabilities.json>
"""

import json
import sys

# Cubelet accepts only this schema.
EXPECTED_SCHEMA = 1
EXPECTED_COMPONENT = "cube-shim"


def fail(message):
    print(f"invalid shim capability declaration: {message}", file=sys.stderr)
    return 1


def main(argv):
    if len(argv) != 2:
        return fail("expected exactly one path argument")
    path = argv[1]

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

    if doc.get("component") != EXPECTED_COMPONENT:
        return fail(
            f"{path} declares component {doc.get('component')!r}, "
            f'expected "{EXPECTED_COMPONENT}"'
        )

    capabilities = doc.get("capabilities")
    if not isinstance(capabilities, dict) or not capabilities:
        return fail(f"{path} declares no capabilities")

    # Levels must be positive integers.
    for name, level in capabilities.items():
        if not isinstance(name, str) or not name:
            return fail(f"{path} has a non-string capability name: {name!r}")
        # bool is a subclass of int.
        if not isinstance(level, int) or isinstance(level, bool) or level < 1:
            return fail(
                f"{path} declares {name} at level {level!r}, "
                "expected a positive integer"
            )

    runtime = doc.get("runtime")
    if runtime is not None and not isinstance(runtime, dict):
        return fail(f"{path} has a non-object runtime section: {runtime!r}")

    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
