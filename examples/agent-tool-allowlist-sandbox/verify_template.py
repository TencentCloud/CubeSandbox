# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Backward-compatible entry: prefer ``python run.py`` (happy path)."""

from __future__ import annotations

from run import main

if __name__ == "__main__":
    main()
