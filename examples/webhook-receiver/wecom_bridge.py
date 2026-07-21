#!/usr/bin/env python3
# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Forward CubeSandbox webhook events to a WeCom (Enterprise WeChat) bot.

Receives CubeSandbox webhooks (optionally verifying the HMAC-SHA256 signature)
and relays a human-readable markdown message to a WeCom group-bot webhook URL.

Set up a group bot in your WeCom group ("Add Bot"), copy its webhook URL, then:

    WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
    CUBE_WEBHOOK_SECRET=my-shared-secret \
    python3 wecom_bridge.py

Point a CubeAPI webhook endpoint at http://<this-host>:9100/webhook.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOST = os.environ.get("HOST", "0.0.0.0")
PORT = int(os.environ.get("PORT", "9100"))
SECRET = os.environ.get("CUBE_WEBHOOK_SECRET", "")
WECOM_BOT_URL = os.environ.get("WECOM_BOT_URL", "")

# Friendly labels for the four sandbox lifecycle events.
EVENT_LABELS = {
    "sandbox.created": "🟢 Sandbox created",
    "sandbox.paused": "🟡 Sandbox paused",
    "sandbox.resumed": "🔵 Sandbox resumed",
    "sandbox.deleted": "🔴 Sandbox deleted",
}


def verify_signature(secret: str, body: bytes, header: str | None) -> bool:
    if not header or not header.startswith("sha256="):
        return False
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header[len("sha256=") :])


def to_markdown(payload: dict) -> str:
    event = payload.get("event", "unknown")
    label = EVENT_LABELS.get(event, f"ℹ️ {event}")
    lines = [f"**{label}**"]
    if "sandbox_id" in payload:
        lines.append(f"> sandbox_id: `{payload['sandbox_id']}`")
    if "template_id" in payload:
        lines.append(f"> template_id: `{payload['template_id']}`")
    if "timestamp" in payload:
        lines.append(f"> time: {payload['timestamp']}")
    return "\n".join(lines)


def send_to_wecom(markdown: str) -> None:
    body = json.dumps({"msgtype": "markdown", "markdown": {"content": markdown}})
    req = urllib.request.Request(
        WECOM_BOT_URL,
        data=body.encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        resp.read()


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)

        if SECRET and not verify_signature(
            SECRET, body, self.headers.get("X-Cube-Signature")
        ):
            self.send_response(401)
            self.end_headers()
            return

        # ACK CubeAPI immediately; forward to WeCom best-effort.
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

        try:
            payload = json.loads(body)
            send_to_wecom(to_markdown(payload))
            print(f"forwarded {payload.get('event')} to WeCom")
        except Exception as exc:  # noqa: BLE001 - example code, log and continue
            print(f"failed to forward to WeCom: {exc}")

    def log_message(self, *_args) -> None:
        pass


def main() -> None:
    if not WECOM_BOT_URL:
        print("error: WECOM_BOT_URL is not set", file=sys.stderr)
        sys.exit(1)
    print(f"WeCom bridge listening on http://{HOST}:{PORT}/webhook")
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        server.shutdown()


if __name__ == "__main__":
    main()
