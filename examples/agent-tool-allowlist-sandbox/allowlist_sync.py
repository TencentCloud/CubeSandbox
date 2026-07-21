# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Emit the demo allowlist as a guest-side text file body.

Single source of truth: allowlist.DEFAULT_ALLOWED_BINARIES.
Used by verify_local.py to keep Dockerfile content aligned.
"""

from __future__ import annotations

from allowlist import DEFAULT_ALLOWED_BINARIES

HEADER = "# Agent tool argv allowlist (demo; host gate is authoritative)"


def allowlist_file_body() -> str:
    names = sorted(DEFAULT_ALLOWED_BINARIES)
    return "\n".join([HEADER, *names]) + "\n"


def dockerfile_run_snippet() -> str:
    """Return the RUN printf fragment that should match Dockerfile."""
    lines = [HEADER, *sorted(DEFAULT_ALLOWED_BINARIES)]
    printf_args = " \\\n        ".join(repr(line) for line in lines)
    return (
        "RUN mkdir -p /etc/cube-sandbox \\\n"
        f"    && printf '%s\\n' \\\n"
        f"        {printf_args} \\\n"
        "    > /etc/cube-sandbox/tool-allowlist.txt\n"
    )


if __name__ == "__main__":
    print(allowlist_file_body(), end="")
