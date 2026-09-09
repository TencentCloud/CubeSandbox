from __future__ import annotations

import asyncio
import json
import os
import unittest
from unittest.mock import AsyncMock, patch

import httpx
from fastapi import HTTPException

import server


class FetchSandboxEnvsTest(unittest.IsolatedAsyncioTestCase):
    async def test_uses_configured_envd_timeout(self) -> None:
        response = httpx.Response(
            200,
            json={},
            request=httpx.Request("GET", server.ENVD_ENVS_URL),
        )
        client = AsyncMock()
        client.get.return_value = response

        with patch.object(server, "ENVD_TIMEOUT", 7.5):
            self.assertEqual(await server._fetch_sandbox_envs(client), {})

        client.get.assert_awaited_once_with(server.ENVD_ENVS_URL, timeout=7.5)

    async def test_returns_envd_environment(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            self.assertEqual(request.url.path, "/envs")
            return httpx.Response(200, json={"BASE": "sandbox", "JSON": '{"n":1}'})

        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            self.assertEqual(
                await server._fetch_sandbox_envs(client),
                {"BASE": "sandbox", "JSON": '{"n":1}'},
            )

    async def test_rejects_non_string_envd_values(self) -> None:
        transport = httpx.MockTransport(
            lambda request: httpx.Response(200, json={"BASE": 1})
        )

        async with httpx.AsyncClient(transport=transport) as client:
            with self.assertRaisesRegex(HTTPException, "invalid environment payload"):
                await server._fetch_sandbox_envs(client)

    async def test_maps_envd_http_failure_to_bad_gateway(self) -> None:
        transport = httpx.MockTransport(
            lambda request: httpx.Response(503, text="not ready")
        )

        async with httpx.AsyncClient(transport=transport) as client:
            with self.assertRaises(HTTPException) as raised:
                await server._fetch_sandbox_envs(client)

        self.assertEqual(raised.exception.status_code, 502)


class KernelContextSandboxEnvsTest(unittest.IsolatedAsyncioTestCase):
    async def test_loads_sandbox_environment_once(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        fetch = AsyncMock(return_value={"BASE": "sandbox", "OVERRIDE": "sandbox"})

        with patch.object(server, "_fetch_sandbox_envs", fetch):
            await context.load_sandbox_envs()
            await context.load_sandbox_envs()

        fetch.assert_awaited_once()
        self.assertEqual(
            context.sandbox_envs,
            {"BASE": "sandbox", "OVERRIDE": "sandbox"},
        )

    async def test_caches_empty_envd_environment(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        fetch = AsyncMock(return_value={})

        with patch.object(server, "_fetch_sandbox_envs", fetch):
            await context.load_sandbox_envs()
            await context.load_sandbox_envs()

        fetch.assert_awaited_once()
        self.assertEqual(context.sandbox_envs, {})

    async def test_retries_after_envd_failure(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        fetch = AsyncMock(
            side_effect=[
                HTTPException(status_code=502, detail="envd unavailable"),
                {"BASE": "sandbox"},
            ]
        )
        with patch.object(server, "_fetch_sandbox_envs", fetch):
            with self.assertRaises(HTTPException):
                await context.load_sandbox_envs()
            await context.load_sandbox_envs()

        self.assertEqual(fetch.await_count, 2)
        self.assertEqual(context.sandbox_envs, {"BASE": "sandbox"})

    async def test_applies_sandbox_environment_only_on_first_execution(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {
            "BASE": "sandbox",
            "OVERRIDE": "sandbox",
        }

        first = context._build_env_setup_code(
            {
                "OVERRIDE": "request",
                "REQUEST_ONLY": "value",
            },
            "_snapshot",
        )
        context.sandbox_envs_applied = True
        second = context._build_env_setup_code(None, None)

        self.assertEqual(
            first.splitlines(),
            [
                'import os; os.environ["BASE"] = "sandbox"',
                'import os; os.environ["OVERRIDE"] = "sandbox"',
                'import os; _snapshot = {key: (key in os.environ, os.environ.get(key)) for key in ["OVERRIDE", "REQUEST_ONLY"]}',
                'import os; os.environ["OVERRIDE"] = "request"',
                'import os; os.environ["REQUEST_ONLY"] = "value"',
            ],
        )
        self.assertEqual(second, "")

    async def test_restores_or_deletes_per_call_environment_after_execution(
        self,
    ) -> None:
        cleanup = server.KernelContext._build_env_cleanup_code("_snapshot")

        self.assertEqual(
            cleanup.splitlines(),
            [
                "def _snapshot_restore():",
                "    import os",
                '    snapshot = globals().pop("_snapshot", {})',
                "    for key, (existed, value) in snapshot.items():",
                "        if existed:",
                "            os.environ[key] = value",
                "        else:",
                "            os.environ.pop(key, None)",
                "_snapshot_restore()",
                "del _snapshot_restore",
            ],
        )

    async def test_executes_cleanup_after_per_call_environment(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {"OVERRIDE": "sandbox"}
        context.ws = AsyncMock()
        requests: list[dict] = []

        async def send(request: str) -> None:
            payload = json.loads(request)
            requests.append(payload)
            msg_id = payload["header"]["msg_id"]
            await context.executions[msg_id].put({"type": "end_of_execution"})

        context.ws.send.side_effect = send

        results = [
            item
            async for item in context.execute(
                "from __future__ import annotations\nprint('ok')",
                env_vars={
                    "OVERRIDE": "request",
                    "REQUEST_ONLY": "value",
                },
            )
        ]
        self.assertEqual(results, [])
        self.assertIsNotNone(context.cleanup_task)
        await context.cleanup_task

        self.assertEqual(len(requests), 3)
        setup_lines = requests[0]["content"]["code"].splitlines()
        snapshot_name = setup_lines[1].split()[2]
        self.assertTrue(snapshot_name.startswith("_cube_lci_env_snapshot_"))
        self.assertEqual(
            setup_lines,
            [
                'import os; os.environ["OVERRIDE"] = "sandbox"',
                f'import os; {snapshot_name} = {{key: (key in os.environ, os.environ.get(key)) for key in ["OVERRIDE", "REQUEST_ONLY"]}}',
                'import os; os.environ["OVERRIDE"] = "request"',
                'import os; os.environ["REQUEST_ONLY"] = "value"',
            ],
        )
        self.assertTrue(requests[0]["content"]["silent"])
        self.assertFalse(requests[0]["content"]["store_history"])
        self.assertEqual(
            requests[1]["content"]["code"],
            "from __future__ import annotations\nprint('ok')",
        )
        self.assertFalse(requests[1]["content"]["silent"])
        self.assertTrue(requests[1]["content"]["store_history"])
        self.assertEqual(
            requests[2]["content"]["code"].splitlines(),
            [
                f"def {snapshot_name}_restore():",
                "    import os",
                f'    snapshot = globals().pop("{snapshot_name}", {{}})',
                "    for key, (existed, value) in snapshot.items():",
                "        if existed:",
                "            os.environ[key] = value",
                "        else:",
                "            os.environ.pop(key, None)",
                f"{snapshot_name}_restore()",
                f"del {snapshot_name}_restore",
            ],
        )
        self.assertTrue(requests[2]["content"]["silent"])
        self.assertFalse(requests[2]["content"]["store_history"])

    async def test_rolls_back_per_call_environment_when_setup_fails(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {"SANDBOX_BASE": "sandbox-value"}
        context.ws = AsyncMock()
        namespace: dict[str, object] = {}
        requests: list[dict] = []

        async def send(request: str) -> None:
            payload = json.loads(request)
            requests.append(payload)
            msg_id = payload["header"]["msg_id"]
            try:
                exec(payload["content"]["code"], namespace)
            except ValueError as exc:
                await context.executions[msg_id].put(
                    {
                        "type": "error",
                        "name": "ValueError",
                        "value": str(exc),
                        "traceback": "",
                    }
                )
            await context.executions[msg_id].put({"type": "end_of_execution"})

        context.ws.send.side_effect = send

        with patch.dict(
            os.environ,
            {"KERNEL_EXISTING": "kernel-value"},
            clear=False,
        ):
            with self.assertRaisesRegex(RuntimeError, "background execution failed"):
                _ = [
                    item
                    async for item in context.execute(
                        "pass",
                        env_vars={
                            "KERNEL_EXISTING": "per-call-value",
                            "PER_CALL_ONLY": "per-call-only",
                            "INVALID_VALUE": "\x00",
                        },
                    )
                ]

            self.assertEqual(os.environ["KERNEL_EXISTING"], "kernel-value")
            self.assertNotIn("PER_CALL_ONLY", os.environ)
            self.assertNotIn("INVALID_VALUE", os.environ)
            self.assertFalse(context.sandbox_envs_applied)

            retry_results = [
                item
                async for item in context.execute(
                    "pass",
                    env_vars=None,
                )
            ]
            self.assertEqual(retry_results, [])
            self.assertTrue(context.sandbox_envs_applied)
            self.assertEqual(len(requests), 4)

    async def test_waits_for_cleanup_before_the_next_execution(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {}
        context.sandbox_envs_applied = True
        context.ws = AsyncMock()
        release_cleanup = asyncio.Event()
        request_sent = asyncio.Event()

        async def cleanup() -> None:
            await release_cleanup.wait()

        async def send(request: str) -> None:
            payload = json.loads(request)
            msg_id = payload["header"]["msg_id"]
            request_sent.set()
            await context.executions[msg_id].put({"type": "end_of_execution"})

        context.cleanup_task = asyncio.create_task(cleanup())
        context.ws.send.side_effect = send

        execution = asyncio.create_task(
            anext(context.execute("print('next')", env_vars=None), None)
        )
        await asyncio.sleep(0)
        self.assertFalse(request_sent.is_set())

        release_cleanup.set()
        await execution
        self.assertTrue(request_sent.is_set())
        self.assertIsNone(context.cleanup_task)

    async def test_restores_kernel_environment_after_per_call_override(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {}
        context.ws = AsyncMock()
        namespace: dict[str, object] = {}

        async def send(request: str) -> None:
            payload = json.loads(request)
            msg_id = payload["header"]["msg_id"]
            exec(payload["content"]["code"], namespace)
            await context.executions[msg_id].put({"type": "end_of_execution"})

        context.ws.send.side_effect = send

        with patch.dict(
            os.environ,
            {"KERNEL_EXISTING": "kernel-value"},
            clear=False,
        ):
            results = [
                item
                async for item in context.execute(
                    "pass",
                    env_vars={
                        "KERNEL_EXISTING": "per-call-value",
                        "PER_CALL_ONLY": "per-call-only",
                    },
                )
            ]
            self.assertEqual(results, [])
            self.assertIsNotNone(context.cleanup_task)
            await context.cleanup_task

            self.assertEqual(os.environ["KERNEL_EXISTING"], "kernel-value")
            self.assertNotIn("PER_CALL_ONLY", os.environ)

    async def test_applies_sandbox_environment_before_invalid_first_code(self) -> None:
        context = server.KernelContext("kernel", "session", "/workspace")
        context.sandbox_envs = {"SANDBOX_BASE": "sandbox-value"}
        context.ws = AsyncMock()
        namespace: dict[str, object] = {}

        async def send(request: str) -> None:
            payload = json.loads(request)
            msg_id = payload["header"]["msg_id"]
            try:
                exec(payload["content"]["code"], namespace)
            except SyntaxError:
                await context.executions[msg_id].put(
                    {
                        "type": "error",
                        "name": "SyntaxError",
                        "value": "invalid syntax",
                        "traceback": "",
                    }
                )
            await context.executions[msg_id].put({"type": "end_of_execution"})

        context.ws.send.side_effect = send

        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("SANDBOX_BASE", None)
            results = [
                item
                async for item in context.execute(
                    "if",
                    env_vars=None,
                )
            ]

            self.assertEqual(results[0]["name"], "SyntaxError")
            self.assertEqual(os.environ["SANDBOX_BASE"], "sandbox-value")
            self.assertTrue(context.sandbox_envs_applied)


if __name__ == "__main__":
    unittest.main()
