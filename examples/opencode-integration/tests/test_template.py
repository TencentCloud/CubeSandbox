# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import re
import unittest
from pathlib import Path

EXAMPLE = Path(__file__).resolve().parents[1]


class TemplateTests(unittest.TestCase):
    def test_dockerfile_does_not_reinstall_inherited_packages(self) -> None:
        dockerfile = (EXAMPLE / "Dockerfile").read_text(encoding="utf-8")
        match = re.search(
            r"apt-get install -y --no-install-recommends(?P<packages>.*?)"
            r"&& rm -rf /var/lib/apt/lists/\*",
            dockerfile,
            flags=re.DOTALL,
        )
        self.assertIsNotNone(match)
        packages = set(re.findall(r"^\s+([a-z0-9-]+)\s*\\?$", match["packages"], re.M))
        self.assertTrue({"git", "jq", "python3", "ripgrep"}.issubset(packages))
        self.assertTrue({"bash", "ca-certificates", "curl"}.isdisjoint(packages))

    def test_template_fails_fast_for_unpinned_architecture(self) -> None:
        dockerfile = (EXAMPLE / "Dockerfile").read_text(encoding="utf-8")
        self.assertTrue(dockerfile.startswith("# syntax=docker/dockerfile:1.8\n"))
        self.assertIn("ARG OPENCODE_VERSION=1.18.9", dockerfile)
        self.assertIn("ARG TARGETARCH", dockerfile)
        self.assertIn("Unsupported TARGETARCH=", dockerfile)
        self.assertIn("opencode-linux-x64.tar.gz", dockerfile)
        self.assertIn(
            "COPY opencode.v1.json /root/.config/opencode/opencode.json",
            dockerfile,
        )

    def test_docker_context_is_an_explicit_allowlist(self) -> None:
        rules = [
            line
            for line in (EXAMPLE / ".dockerignore")
            .read_text(encoding="utf-8")
            .splitlines()
            if line and not line.startswith("#")
        ]
        self.assertEqual(rules, ["*", "!Dockerfile", "!opencode.v1.json"])


if __name__ == "__main__":
    unittest.main()
