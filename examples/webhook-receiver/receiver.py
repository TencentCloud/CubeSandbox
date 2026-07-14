#!/usr/bin/env python3
"""Development receiver for CubeAPI lifecycle Webhooks."""

import hashlib
import hmac
import json
import os
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SECRET = os.getenv("CUBE_WEBHOOK_SECRET", "")
HOST = os.getenv("WEBHOOK_RECEIVER_HOST", "127.0.0.1")
PORT = int(os.getenv("WEBHOOK_RECEIVER_PORT", "8088"))
WECOM_BOT_URL = os.getenv("WECOM_BOT_URL", "")


MAX_SIGNATURE_AGE_SECONDS = 300


def valid_signature(
    body: bytes, supplied: str, timestamp: str, now: int | None = None
) -> bool:
    if not SECRET:
        return True
    try:
        signed_at = int(timestamp)
    except (TypeError, ValueError):
        return False
    current_time = int(time.time()) if now is None else now
    if abs(current_time - signed_at) > MAX_SIGNATURE_AGE_SECONDS:
        return False
    signed_payload = timestamp.encode() + b"." + body
    expected = "sha256=" + hmac.new(
        SECRET.encode(), signed_payload, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(supplied, expected)


def forward_to_wecom(event: dict) -> None:
    if not WECOM_BOT_URL:
        return
    content = f"CubeSandbox {event['event']}: {event['sandbox_id']}"
    request = urllib.request.Request(
        WECOM_BOT_URL,
        json.dumps({"msgtype": "text", "text": {"content": content}}).encode(),
        {"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=5):
        pass


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/webhook":
            self.send_error(404)
            return
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if not valid_signature(
            body,
            self.headers.get("X-Cube-Signature-256", ""),
            self.headers.get("X-Cube-Timestamp", ""),
        ):
            self.send_error(401, "invalid signature")
            return
        try:
            event = json.loads(body)
            if not all(key in event for key in ("event", "timestamp", "sandbox_id")):
                raise ValueError("missing required event field")
            if self.headers.get("X-Cube-Event") != event["event"]:
                raise ValueError("event header does not match payload")
            forward_to_wecom(event)
        except (json.JSONDecodeError, ValueError) as error:
            self.send_error(400, str(error))
            return
        except OSError as error:
            self.send_error(502, f"WeCom forwarding failed: {error}")
            return
        print(json.dumps(event, ensure_ascii=False), flush=True)
        self.send_response(204)
        self.end_headers()

    def log_message(self, format, *args):
        return


if __name__ == "__main__":
    print(f"listening on http://{HOST}:{PORT}/webhook", flush=True)
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
