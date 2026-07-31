# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0


import unittest

from app import normalize_slug


class NormalizeSlugTests(unittest.TestCase):
    def test_normalizes_words_and_symbols(self):
        self.assertEqual(
            normalize_slug("  Hello, Cube Sandbox!  "),
            "hello-cube-sandbox",
        )

    def test_collapses_separators(self):
        self.assertEqual(normalize_slug("MiMo___Code---Agent"), "mimo-code-agent")

    def test_preserves_ascii_digits(self):
        self.assertEqual(normalize_slug("Release 2.5 RC1"), "release-2-5-rc1")

    def test_handles_empty_input(self):
        self.assertEqual(normalize_slug("  !!!  "), "")


if __name__ == "__main__":
    unittest.main()
