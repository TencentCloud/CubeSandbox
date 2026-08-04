# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import hashlib
import hmac
import http.client
import io
import json
import threading
import unittest
from contextlib import redirect_stderr, redirect_stdout

from receiver import ReceiverConfig, WebhookServer, validate_payload, verify_signature


class ReceiverTest(unittest.TestCase):
    def test_verifies_exact_body_signature(self) -> None:
        body = b'{"event":"sandbox.created"}'
        signature = "sha256=" + hmac.new(
            b"secret", body, hashlib.sha256
        ).hexdigest()

        self.assertTrue(verify_signature(body, signature, "secret"))
        self.assertFalse(verify_signature(body + b" ", signature, "secret"))

    def test_requires_core_payload_fields(self) -> None:
        payload = {
            "event": "sandbox.deleted",
            "timestamp": "2026-07-10T00:00:00Z",
            "sandbox_id": "sb-1",
        }
        self.assertEqual(validate_payload(payload), payload)
        with self.assertRaisesRegex(ValueError, "sandbox_id"):
            validate_payload({"event": "sandbox.deleted", "timestamp": "now"})

    def test_http_server_accepts_a_signed_event(self) -> None:
        server = WebhookServer(
            ("127.0.0.1", 0), ReceiverConfig(secret="secret", wecom_url="")
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            payload = {
                "event": "sandbox.created",
                "timestamp": "2026-07-10T00:00:00Z",
                "sandbox_id": "sb-http",
            }
            body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
            signature = "sha256=" + hmac.new(
                b"secret", body, hashlib.sha256
            ).hexdigest()
            connection = http.client.HTTPConnection(
                "127.0.0.1", server.server_port, timeout=2
            )
            try:
                with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    connection.request(
                        "POST",
                        "/webhook",
                        body=body,
                        headers={"X-CubeSandbox-Signature": signature},
                    )
                    response = connection.getresponse()
                    response.read()
                    self.assertEqual(response.status, 204)
            finally:
                connection.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


if __name__ == "__main__":
    unittest.main()
