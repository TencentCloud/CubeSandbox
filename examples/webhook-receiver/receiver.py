#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Minimal CubeSandbox webhook receiver with optional WeCom forwarding."""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import sys
import urllib.request
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from socketserver import TCPServer
from typing import Any, Optional


MAX_BODY_BYTES = 1024 * 1024


def verify_signature(body: bytes, signature: Optional[str], secret: str) -> bool:
    if not secret:
        return True
    if not signature:
        return False
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


def validate_payload(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise ValueError("payload must be a JSON object")
    for field in ("event", "timestamp", "sandbox_id"):
        if not isinstance(payload.get(field), str) or not payload[field]:
            raise ValueError(f"payload field {field!r} must be a non-empty string")
    return payload


def forward_to_wecom(url: str, payload: dict[str, Any]) -> None:
    template = payload.get("template_id", "n/a")
    content = (
        f"CubeSandbox {payload['event']}\n"
        f"sandbox: {payload['sandbox_id']}\n"
        f"template: {template}\n"
        f"timestamp: {payload['timestamp']}"
    )
    body = json.dumps(
        {"msgtype": "text", "text": {"content": content}},
        separators=(",", ":"),
    ).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        result = json.loads(response.read())
    if result.get("errcode") != 0:
        raise RuntimeError(f"WeCom returned errcode={result.get('errcode')}")


@dataclass(frozen=True)
class ReceiverConfig:
    secret: str
    wecom_url: str


class WebhookServer(ThreadingHTTPServer):
    daemon_threads = True

    def server_bind(self) -> None:
        # HTTPServer performs a reverse-DNS lookup during bind. That lookup is
        # unnecessary for this receiver and can delay startup on minimal hosts.
        TCPServer.server_bind(self)
        host, port = self.server_address[:2]
        self.server_name = host
        self.server_port = port

    def __init__(self, address: tuple[str, int], config: ReceiverConfig) -> None:
        super().__init__(address, WebhookHandler)
        self.receiver_config = config


class WebhookHandler(BaseHTTPRequestHandler):
    server: WebhookServer

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        if self.path != "/webhook":
            self.send_error(HTTPStatus.NOT_FOUND)
            return

        try:
            content_length = int(self.headers.get("Content-Length", ""))
        except ValueError:
            self.send_error(HTTPStatus.BAD_REQUEST, "invalid Content-Length")
            return
        if content_length <= 0 or content_length > MAX_BODY_BYTES:
            self.send_error(HTTPStatus.REQUEST_ENTITY_TOO_LARGE)
            return

        body = self.rfile.read(content_length)
        config = self.server.receiver_config
        if not verify_signature(
            body, self.headers.get("X-CubeSandbox-Signature"), config.secret
        ):
            self.send_error(HTTPStatus.UNAUTHORIZED, "invalid webhook signature")
            return

        try:
            payload = validate_payload(json.loads(body))
        except (json.JSONDecodeError, UnicodeDecodeError, ValueError) as error:
            self.send_error(HTTPStatus.BAD_REQUEST, str(error))
            return

        delivery_id = self.headers.get("X-CubeSandbox-Delivery", "")
        print(
            json.dumps(
                {"delivery_id": delivery_id, "payload": payload},
                ensure_ascii=False,
                separators=(",", ":"),
            ),
            flush=True,
        )

        if config.wecom_url:
            try:
                forward_to_wecom(config.wecom_url, payload)
            except Exception as error:  # The 502 asks CubeAPI to retry delivery.
                print(
                    f"WeCom forwarding failed: {type(error).__name__}",
                    file=sys.stderr,
                    flush=True,
                )
                self.send_error(HTTPStatus.BAD_GATEWAY, "WeCom forwarding failed")
                return

        self.send_response(HTTPStatus.NO_CONTENT)
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        print(
            f"{self.client_address[0]} - {format % args}",
            file=sys.stderr,
            flush=True,
        )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--host", default=os.getenv("WEBHOOK_LISTEN_HOST", "127.0.0.1")
    )
    parser.add_argument(
        "--port", type=int, default=int(os.getenv("WEBHOOK_LISTEN_PORT", "8088"))
    )
    args = parser.parse_args()

    config = ReceiverConfig(
        secret=os.getenv("WEBHOOK_SECRET", ""),
        wecom_url=os.getenv("WECOM_BOT_URL", ""),
    )
    if not config.secret:
        print(
            "warning: WEBHOOK_SECRET is empty; signature verification is disabled",
            file=sys.stderr,
        )

    server = WebhookServer((args.host, args.port), config)
    print(f"listening on http://{args.host}:{args.port}/webhook", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
