#!/usr/bin/env python3
"""Minimal CubeSandbox webhook receiver using only the Python standard library."""

import hashlib
import hmac
import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SECRET = os.environ.get("WEBHOOK_SECRET", "")
MAX_TIMESTAMP_AGE_SECONDS = 300


class WebhookHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if SECRET and not self.verify_signature(body):
            self.send_error(401, "invalid webhook signature")
            return

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "invalid JSON")
            return

        print(json.dumps(payload, indent=2, ensure_ascii=False), flush=True)
        self.send_response(204)
        self.end_headers()

    def verify_signature(self, body):
        timestamp = self.headers.get("X-Cube-Webhook-Timestamp", "")
        signature = self.headers.get("X-Cube-Webhook-Signature", "")
        try:
            timestamp_value = int(timestamp)
        except ValueError:
            return False
        if abs(time.time() - timestamp_value) > MAX_TIMESTAMP_AGE_SECONDS:
            return False

        signed_payload = timestamp.encode() + b"." + body
        expected = "sha256=" + hmac.new(
            SECRET.encode(), signed_payload, hashlib.sha256
        ).hexdigest()
        return hmac.compare_digest(expected, signature)

    def log_message(self, format, *args):
        print("receiver:", format % args, flush=True)


if __name__ == "__main__":
    address = os.environ.get("WEBHOOK_BIND", "0.0.0.0")
    port = int(os.environ.get("WEBHOOK_PORT", "8080"))
    print(f"Listening on http://{address}:{port}", flush=True)
    ThreadingHTTPServer((address, port), WebhookHandler).serve_forever()
