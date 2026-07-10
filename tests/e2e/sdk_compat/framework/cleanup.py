# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from adapters.api_adapter import ApiClient
from adapters.base import SandboxAdapter

from framework.config import SdkE2EConfig


def safe_kill(adapter: SandboxAdapter, config: SdkE2EConfig) -> list[str]:
    """Best-effort sandbox cleanup.

    Returns diagnostic messages instead of raising, so teardown never hides the
    original test failure.
    """

    errors: list[str] = []
    try:
        adapter.kill()
    except Exception as exc:  # noqa: BLE001 - teardown must be best-effort
        errors.append(f"{adapter.backend}.kill failed for {adapter.sandbox_id}: {exc}")
        api = ApiClient(config)
        try:
            api.delete_sandbox(adapter.sandbox_id)
        except Exception as api_exc:  # noqa: BLE001
            errors.append(f"REST delete failed for {adapter.sandbox_id}: {api_exc}")
        finally:
            api.close()
    finally:
        try:
            adapter.close()
        except Exception as exc:  # noqa: BLE001
            errors.append(f"{adapter.backend}.close failed for {adapter.sandbox_id}: {exc}")
    return errors
