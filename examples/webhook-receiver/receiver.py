#!/usr/bin/env python3
"""Minimal CubeSandbox webhook receiver.

The server prints every verified payload as JSON. When WECOM_BOT_WEBHOOK is set,
it also forwards a compact markdown alert to a WeCom group robot.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SECRET = os.getenv("WEBHOOK_SECRET", "")
WECOM_BOT_WEBHOOK = os.getenv("WECOM_BOT_WEBHOOK", "")


def verify_signature(body: bytes, signature: str | None) -> bool:
    if not SECRET:
        return True
    if not signature or not signature.startswith("sha256="):
        return False
    expected = "sha256=" + hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)


def forward_to_wecom(payload: dict) -> None:
    if not WECOM_BOT_WEBHOOK:
        return
    text = (
        f"**CubeSandbox {payload.get('event', 'event')}**\n"
        f"> sandbox: `{payload.get('sandbox_id', '-')}`\n"
        f"> template: `{payload.get('template_id', '-')}`\n"
        f"> time: `{payload.get('timestamp', '-')}`"
    )
    data = json.dumps({"msgtype": "markdown", "markdown": {"content": text}}).encode()
    req = urllib.request.Request(
        WECOM_BOT_WEBHOOK,
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            resp.read()
    except urllib.error.URLError as err:
        print(f"wecom forward failed: {err}", file=sys.stderr)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)

        if not verify_signature(body, self.headers.get("X-Cube-Signature")):
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"invalid signature\n")
            return

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"invalid json\n")
            return

        print(json.dumps(payload, ensure_ascii=False, sort_keys=True), flush=True)
        forward_to_wecom(payload)
        self.send_response(204)
        self.end_headers()

    def log_message(self, fmt: str, *args: object) -> None:
        print(f"{self.address_string()} - {fmt % args}", file=sys.stderr)


def main() -> None:
    host = os.getenv("WEBHOOK_HOST", "0.0.0.0")
    port = int(os.getenv("WEBHOOK_PORT", "9000"))
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"listening on http://{host}:{port}/webhook", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
