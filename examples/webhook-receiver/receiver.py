#!/usr/bin/env python3
"""Minimal receiver for CubeAPI Webhook lifecycle events."""

import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SECRET = os.environ.get("CUBE_WEBHOOK_SECRET", "")
HOST = os.environ.get("WEBHOOK_RECEIVER_HOST", "127.0.0.1")
PORT = int(os.environ.get("WEBHOOK_RECEIVER_PORT", "8088"))


class WebhookReceiver(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(content_length)
        signature = self.headers.get("X-Cube-Signature-256", "")

        if SECRET:
            expected = "sha256=" + hmac.new(
                SECRET.encode(), body, hashlib.sha256
            ).hexdigest()
            if not hmac.compare_digest(signature, expected):
                self.send_error(401, "invalid webhook signature")
                return

        try:
            event = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "invalid JSON")
            return

        print(json.dumps(event, ensure_ascii=False), flush=True)
        self.send_response(204)
        self.end_headers()

    def log_message(self, format, *args):
        return


if __name__ == "__main__":
    print(f"listening on http://{HOST}:{PORT}/webhook", flush=True)
    ThreadingHTTPServer((HOST, PORT), WebhookReceiver).serve_forever()
