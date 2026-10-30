import unittest
from unittest.mock import AsyncMock, patch

import httpx
import server


class ContextStartupTest(unittest.IsolatedAsyncioTestCase):
    async def test_failed_connection_cleanup_still_deletes_known_session(self):
        client = AsyncMock()
        client.post.return_value = httpx.Response(200, json={"kernel": {"id": "kernel"}, "id": "session"})
        kernel = AsyncMock()
        kernel.connect.side_effect = TimeoutError("handshake timed out")
        kernel.close.side_effect = OSError("socket close failed")
        with patch.object(server, "client", client), \
             patch.object(server, "_resolve_cwd", return_value="/workspace"), \
             patch.object(server, "KernelContext", return_value=kernel), \
             patch.object(server, "contexts", {}):
            with self.assertRaisesRegex(TimeoutError, "handshake timed out"):
                await server.create_context()
        client.delete.assert_awaited_once_with(f"{server.JUPYTER_BASE_URL}/api/sessions/session")

    async def test_kernel_handshake_has_explicit_timeout(self):
        kernel = server.KernelContext("kernel", "session", "/workspace")
        ws = AsyncMock()
        with patch.object(server, "connect", AsyncMock(return_value=ws)) as connect, \
             patch.object(server, "CONTEXT_CREATE_TIMEOUT", 75), \
             patch.object(kernel, "_receive_messages", AsyncMock()), \
             patch.object(kernel, "_run_background", AsyncMock()):
            await kernel.connect()
            self.assertEqual(connect.await_args.kwargs["open_timeout"], 75)
            await kernel.close()

    async def test_failed_handshake_cleans_known_session(self):
        client = AsyncMock()
        client.post.return_value = httpx.Response(200, json={"kernel": {"id": "kernel"}, "id": "session"})
        kernel = AsyncMock()
        kernel.connect.side_effect = TimeoutError("handshake timed out")
        with patch.object(server, "client", client), \
             patch.object(server, "_resolve_cwd", return_value="/workspace"), \
             patch.object(server, "KernelContext", return_value=kernel), \
             patch.object(server, "contexts", {}) as contexts:
            with self.assertRaises(TimeoutError):
                await server.create_context()
            self.assertEqual(contexts, {})
        kernel.close.assert_awaited_once()
        client.delete.assert_awaited_once_with(f"{server.JUPYTER_BASE_URL}/api/sessions/session")
        client.post.assert_awaited_once()

    async def test_context_creation_uses_explicit_timeout(self):
        client = AsyncMock()
        client.post.return_value = httpx.Response(
            200, json={"kernel": {"id": "kernel"}, "id": "session"}
        )
        kernel = AsyncMock()
        with patch.object(server, "client", client), \
             patch.object(server, "CONTEXT_CREATE_TIMEOUT", 75), \
             patch.object(server, "_resolve_cwd", return_value="/workspace"), \
             patch.object(server, "KernelContext", return_value=kernel), \
             patch.object(server, "contexts", {}):
            result = await server.create_context()
        self.assertEqual(result.id, "kernel")
        self.assertEqual(client.post.await_args.kwargs["timeout"], 75)
        client.post.assert_awaited_once()
        kernel.connect.assert_awaited_once()

    async def test_timeout_does_not_repeat_session_creation(self):
        client = AsyncMock()
        client.post.side_effect = httpx.ReadTimeout("kernel startup slow")
        with patch.object(server, "client", client), \
             patch.object(server, "_resolve_cwd", return_value="/workspace"):
            with self.assertRaises(httpx.ReadTimeout):
                await server.create_context()
        client.post.assert_awaited_once()

    async def test_startup_failure_closes_http_client(self):
        client = AsyncMock()
        with patch.object(server, "_wait_for_jupyter", AsyncMock()), \
             patch.object(server, "_wait_for_envd", AsyncMock()), \
             patch.object(server.httpx, "AsyncClient", return_value=client), \
             patch.object(server, "contexts", {}), \
             patch.object(server, "create_context", AsyncMock(side_effect=httpx.ReadTimeout("slow"))):
            with self.assertRaises(httpx.ReadTimeout):
                async with server.lifespan(server.app):
                    self.fail("failed startup must not become ready")
        client.aclose.assert_awaited_once()
        self.assertIsNone(server.default_context_id)

    async def test_missing_envd_never_reports_interpreter_ready(self):
        client = AsyncMock()
        create = AsyncMock()
        with patch.object(server, "_wait_for_jupyter", AsyncMock()), \
             patch.object(server, "_wait_for_envd", AsyncMock(side_effect=RuntimeError("envd did not become ready"))), \
             patch.object(server.httpx, "AsyncClient", return_value=client), \
             patch.object(server, "contexts", {}), \
             patch.object(server, "create_context", create):
            with self.assertRaisesRegex(RuntimeError, "envd did not become ready"):
                async with server.lifespan(server.app):
                    self.fail("missing envd must not become ready")
        create.assert_not_awaited()
        client.aclose.assert_awaited_once()
