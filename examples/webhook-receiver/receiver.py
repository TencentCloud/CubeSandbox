#!/usr/bin/env python3
"""Small dependency-free CubeSandbox webhook receiver for local verification."""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MAX_BODY_BYTES = 1024 * 1024


class WebhookHandler(BaseHTTPRequestHandler):
    secret: bytes | None = None
    webhook_path = "/webhook"

    def do_POST(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if self.path != self.webhook_path:
            self.send_error(404, "not found")
            return

        try:
            content_length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400, "invalid content length")
            return
        if content_length < 0 or content_length > MAX_BODY_BYTES:
            self.send_error(413, "payload too large")
            return

        body = self.rfile.read(content_length)
        if self.secret is not None:
            provided = self.headers.get("X-Cube-Signature-256", "")
            expected = "sha256=" + hmac.new(
                self.secret, body, hashlib.sha256
            ).hexdigest()
            if not hmac.compare_digest(provided, expected):
                self.send_error(401, "invalid webhook signature")
                return

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "payload is not valid JSON")
            return
        if not isinstance(payload, dict):
            self.send_error(400, "payload must be a JSON object")
            return

        missing = [
            field
            for field in ("event", "timestamp", "sandbox_id")
            if not payload.get(field)
        ]
        if missing:
            self.send_error(400, "missing fields: " + ", ".join(missing))
            return

        print(json.dumps(payload, ensure_ascii=False, sort_keys=True), flush=True)
        self.send_response(204)
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        print(f"[receiver] {format % args}", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default=os.getenv("WEBHOOK_RECEIVER_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.getenv("WEBHOOK_RECEIVER_PORT", "8099")))
    parser.add_argument("--path", default=os.getenv("WEBHOOK_RECEIVER_PATH", "/webhook"))
    parser.add_argument(
        "--secret",
        default=os.getenv("WEBHOOK_RECEIVER_SECRET"),
        help="shared secret; also read from WEBHOOK_RECEIVER_SECRET",
    )
    args = parser.parse_args()

    WebhookHandler.secret = args.secret.encode() if args.secret else None
    WebhookHandler.webhook_path = args.path
    server = ThreadingHTTPServer((args.host, args.port), WebhookHandler)
    print(
        f"[receiver] listening on http://{args.host}:{args.port}{args.path} "
        f"(hmac={'enabled' if WebhookHandler.secret else 'disabled'})",
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("[receiver] stopping", flush=True)
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
