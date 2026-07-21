# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Local verification gate for this example (no Cube deploy required by default).

Runs:
  1) unit tests
  2) run_denied.py
  3) Dockerfile allowlist drift check vs allowlist.py
  4) optional: docker image markers if IMAGE_TAG exists
  5) optional: run_allowlisted.py if CUBE_TEMPLATE_ID + E2B_API_URL are set
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import unittest
from pathlib import Path

from allowlist_sync import HEADER, allowlist_file_body

ROOT = Path(__file__).resolve().parent


def _run(cmd: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    print("+", " ".join(cmd))
    return subprocess.run(
        cmd,
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=check,
    )


def step_unit_tests() -> None:
    loader = unittest.TestLoader()
    suite = loader.discover(str(ROOT), pattern="test_allowlist.py")
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if not result.wasSuccessful():
        raise SystemExit("unit tests failed")


def step_deny() -> None:
    proc = _run([sys.executable, "run_denied.py"])
    out = (proc.stdout or "") + (proc.stderr or "")
    print(out.strip())
    if "denied_as_expected" not in out:
        raise SystemExit("run_denied.py did not print denied_as_expected")


def step_dockerfile_sync() -> None:
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    expected_names = []
    for line in allowlist_file_body().splitlines():
        if line.startswith("#") or not line.strip():
            continue
        expected_names.append(line.strip())

    # Extract single-quoted names from the printf block after the header comment.
    block = re.search(
        r"printf '%s\\n' \\\n(?P<body>.*?)\n\s*> /etc/cube-sandbox/tool-allowlist.txt",
        dockerfile,
        flags=re.S,
    )
    if not block:
        raise SystemExit("Dockerfile printf allowlist block not found")

    names = re.findall(r"'([^']+)'", block.group("body"))
    # Drop the header string if present as a printf arg.
    names = [n for n in names if n != HEADER and not n.startswith("#")]
    if names != expected_names:
        raise SystemExit(
            "Dockerfile allowlist drifted from allowlist.py:\n"
            f"  dockerfile={names}\n"
            f"  expected ={expected_names}\n"
            "Update Dockerfile or DEFAULT_ALLOWED_BINARIES, then re-run."
        )
    print("dockerfile_sync=OK", names)


def step_docker_markers_optional() -> None:
    image = os.environ.get("ALLOWLIST_IMAGE_TAG", "agent-tool-allowlist-sandbox:night-verified")
    inspect = subprocess.run(
        ["docker", "image", "inspect", image],
        capture_output=True,
        text=True,
    )
    if inspect.returncode != 0:
        print(f"docker_markers=SKIP (image {image!r} not present)")
        return

    env_proc = _run(
        [
            "docker",
            "image",
            "inspect",
            image,
            "--format",
            "{{range .Config.Env}}{{println .}}{{end}}",
        ]
    )
    if "TOOL_ALLOWLIST_SANDBOX=1" not in env_proc.stdout:
        raise SystemExit("image missing TOOL_ALLOWLIST_SANDBOX=1")

    file_proc = _run(
        [
            "docker",
            "run",
            "--rm",
            "--entrypoint",
            "/bin/sh",
            image,
            "-c",
            "cat /etc/cube-sandbox/tool-allowlist.txt",
        ]
    )
    if file_proc.stdout != allowlist_file_body():
        raise SystemExit(
            "image allowlist file drifted from allowlist.py:\n"
            f"image:\n{file_proc.stdout!r}\n"
            f"expected:\n{allowlist_file_body()!r}"
        )
    print(f"docker_markers=OK ({image})")


def step_allowlisted_optional() -> None:
    if not os.environ.get("CUBE_TEMPLATE_ID") or not os.environ.get("E2B_API_URL"):
        print("allowlisted=SKIP (CUBE_TEMPLATE_ID / E2B_API_URL not set)")
        return

    os.environ.setdefault("E2B_API_KEY", "e2b_000000")
    use_sidecar = os.environ.get("ALLOWLIST_USE_SIDECAR", "").lower() in {
        "1",
        "true",
        "yes",
        "on",
    }
    script = "run_allowlisted_sidecar.py" if use_sidecar else "run_allowlisted.py"
    proc = _run([sys.executable, script], check=False)
    out = (proc.stdout or "") + (proc.stderr or "")
    print(out.strip())
    if proc.returncode != 0:
        raise SystemExit(f"{script} failed with exit {proc.returncode}")
    if "agent-tool-allowlist-ok" not in out or "artifact: artifact-ok" not in out:
        raise SystemExit(f"{script} output missing expected markers")
    print(f"allowlisted=OK ({script})")


def main() -> None:
    step_unit_tests()
    step_deny()
    step_dockerfile_sync()
    step_docker_markers_optional()
    step_allowlisted_optional()
    print("verify_local=ALL_GREEN")


if __name__ == "__main__":
    main()
