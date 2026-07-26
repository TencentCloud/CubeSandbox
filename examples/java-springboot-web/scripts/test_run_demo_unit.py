# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import unittest

import run_demo


class RunDemoCommandTest(unittest.TestCase):
    def test_maven_cache_check_follows_repository_symlink(self) -> None:
        command = run_demo._maven_cache_check_command()

        self.assertIn(
            f"find -L {run_demo.REMOTE_MAVEN_REPO} -type f",
            command,
        )


if __name__ == "__main__":
    unittest.main()
