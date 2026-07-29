#!/usr/bin/env python3
"""Minimal CubeSandbox Webhook receiver with optional HMAC verification."""

import hashlib
import hmac
import json
import os
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SECRET = os.environ.get("CUBE_WEBHOOK_SECRET", "")
MAX_AGE_SECONDS = int(os.environ.get("CUBE_WEBHOOK_MAX_AGE_SECONDS", "300"))
MAX_FUTURE_SKEW_SECONDS = 30
MAX_BODY_BYTES = 1_048_576
READ_TIMEOUT_SECONDS = 30


class Receiver(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.request.settimeout(READ_TIMEOUT_SECONDS)

    def do_POST(self):
        if self.path != "/webhooks/cube":
            self.send_error(404, "not found")
            return
        try:
            size = int(self.headers.get("Content-Length", ""))
        except ValueError:
            self.send_error(400, "invalid Content-Length")
            return
        if size < 0:
            self.send_error(400, "invalid Content-Length")
            return
        if size > MAX_BODY_BYTES:
            self.send_error(413, "payload too large")
            return
        try:
            body = self.rfile.read(size)
        except TimeoutError:
            self.send_error(408, "request body timeout")
            return
        if len(body) != size:
            self.send_error(400, "incomplete request body")
            return
        received = self.headers.get("X-Cube-Signature-256", "")
        if SECRET:
            expected = "sha256=" + hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()
            if not hmac.compare_digest(received, expected):
                self.send_error(401, "invalid webhook signature")
                return
        try:
            event = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "invalid JSON")
            return
        if not isinstance(event, dict):
            self.send_error(400, "invalid event JSON")
            return
        timestamp = event.get("timestamp")
        event_name = event.get("event")
        header_timestamp = self.headers.get("X-Cube-Timestamp", "")
        header_event = self.headers.get("X-Cube-Event", "")
        if not isinstance(event_name, str) or header_event != event_name:
            self.send_error(401, "webhook event mismatch")
            return
        if not isinstance(timestamp, str):
            self.send_error(401, "invalid webhook timestamp")
            return
        try:
            observed = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            header_observed = datetime.fromisoformat(
                header_timestamp.replace("Z", "+00:00")
            )
            if observed.tzinfo is None or header_observed.tzinfo is None:
                raise ValueError("timestamp must include a timezone")
        except (TypeError, ValueError):
            self.send_error(401, "invalid webhook timestamp")
            return
        if header_observed != observed:
            self.send_error(401, "webhook timestamp mismatch")
            return
        age = (datetime.now(timezone.utc) - observed).total_seconds()
        if age > MAX_AGE_SECONDS or age < -MAX_FUTURE_SKEW_SECONDS:
            self.send_error(401, "stale webhook timestamp")
            return
        print(json.dumps(event, ensure_ascii=False))
        self.send_response(204)
        self.end_headers()

    def log_message(self, format, *args):
        return


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "9010"))
    print(f"listening on http://127.0.0.1:{port}/webhooks/cube")
    ThreadingHTTPServer(("127.0.0.1", port), Receiver).serve_forever()
