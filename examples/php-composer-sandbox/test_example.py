from __future__ import annotations

import unittest
from unittest.mock import patch

from common import application_url, template_id, wait_for_health


class _Sandbox:
    def get_host(self, port: int) -> str:
        return f"{port}-sandbox-123.cube.local"


class _Response:
    def __init__(self, status_code: int, payload: dict[str, str]):
        self.status_code = status_code
        self._payload = payload

    def json(self) -> dict[str, str]:
        return self._payload


class ExampleHelperTests(unittest.TestCase):
    def test_application_url_uses_sandbox_port(self) -> None:
        self.assertEqual(application_url(_Sandbox()), "http://8080-sandbox-123.cube.local")

    def test_template_id_rejects_placeholder(self) -> None:
        with patch.dict("os.environ", {"CUBE_TEMPLATE_ID": "tpl_replace_me"}, clear=True):
            with self.assertRaisesRegex(RuntimeError, "CUBE_TEMPLATE_ID"):
                template_id()

    def test_wait_for_health_retries_until_valid_payload(self) -> None:
        responses = iter(
            [
                _Response(503, {}),
                _Response(200, {"status": "ok", "runtime": "php"}),
            ]
        )
        with patch("common.time.sleep") as sleep:
            payload = wait_for_health(
                lambda *_args, **_kwargs: next(responses),
                "http://sandbox.example",
                attempts=2,
                delay_seconds=0,
            )
        self.assertEqual(payload["runtime"], "php")
        sleep.assert_called_once_with(0)


if __name__ == "__main__":
    unittest.main()
