#!/usr/bin/env python3
"""Minimal CubeAPI webhook receiver using only the Python standard library."""

from __future__ import annotations

import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Optional


HOST = "127.0.0.1"
PORT = 18080
PATH = "/webhook"

HEADER_EVENT = "X-Cube-Webhook-Event"
HEADER_DELIVERY = "X-Cube-Webhook-Delivery"
HEADER_TIMESTAMP = "X-Cube-Webhook-Timestamp"
HEADER_SIGNATURE = "X-Cube-Webhook-Signature"


class WebhookHandler(BaseHTTPRequestHandler):
    server_version = "CubeWebhookReceiver/1.0"

    def do_POST(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if self.path != PATH:
            self._respond(404, "not found\n")
            return

        raw_body = self._read_body()
        if raw_body is None:
            return

        try:
            payload: Any = json.loads(raw_body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._respond(400, "invalid JSON\n")
            return

        event = self.headers.get(HEADER_EVENT, "")
        delivery_id = self.headers.get(HEADER_DELIVERY, "")
        timestamp = self.headers.get(HEADER_TIMESTAMP, "")

        secret = os.environ.get("WEBHOOK_SECRET")
        if secret is not None:
            supplied_signature = self.headers.get(HEADER_SIGNATURE, "")
            if not timestamp or not delivery_id or not supplied_signature:
                self._respond(401, "invalid signature\n")
                return
            expected_signature = self._signature(
                secret, timestamp, delivery_id, raw_body
            )
            if not hmac.compare_digest(supplied_signature, expected_signature):
                self._respond(401, "invalid signature\n")
                return

        print(f"event: {event}")
        print(f"delivery_id: {delivery_id}")
        print(f"timestamp: {timestamp}")
        print("payload:")
        print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))
        print(flush=True)

        self._respond(200, "ok\n")

    def _read_body(self) -> Optional[bytes]:
        content_length = self.headers.get("Content-Length")
        try:
            length = int(content_length) if content_length is not None else -1
        except ValueError:
            length = -1

        if length < 0:
            self._respond(400, "invalid Content-Length\n")
            return None
        return self.rfile.read(length)

    @staticmethod
    def _signature(
        secret: str, timestamp: str, delivery_id: str, raw_body: bytes
    ) -> str:
        signing_input = (
            timestamp.encode("utf-8")
            + b"."
            + delivery_id.encode("utf-8")
            + b"."
            + raw_body
        )
        digest = hmac.new(
            secret.encode("utf-8"), signing_input, hashlib.sha256
        ).hexdigest()
        return f"v1={digest}"

    def _respond(self, status: int, message: str) -> None:
        body = message.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main() -> None:
    server = ThreadingHTTPServer((HOST, PORT), WebhookHandler)
    print(f"CubeAPI webhook receiver listening on http://{HOST}:{PORT}{PATH}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
