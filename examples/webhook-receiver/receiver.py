#!/usr/bin/env python3
"""Standalone webhook receiver for CubeAPI webhook events.

Start the receiver:

    python3 receiver.py                 # listen on :18080
    WEBHOOK_PORT=9090 python3 receiver.py

Enable HMAC verification:

    WEBHOOK_SECRET=your-secret python3 receiver.py

Then configure CubeAPI to send webhooks to http://<host>:<port>/webhook.
"""

import hashlib
import hmac
import http.server
import json
import os
from datetime import datetime
from typing import Optional


PORT = int(os.environ.get("WEBHOOK_PORT", "18080"))
HMAC_SECRET = os.environ.get("WEBHOOK_SECRET", "")

SIGNATURE_HEADER = "X-Cube-Webhook-Signature"


def verify_hmac(raw_body: bytes, header_value: Optional[str]) -> bool:
    """Return True if the HMAC header matches the raw body bytes."""
    if not HMAC_SECRET:
        return True  # verification is disabled when no secret is configured
    if not header_value or not header_value.startswith("sha256="):
        return False
    expected = hmac.new(
        HMAC_SECRET.encode(), raw_body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(f"sha256={expected}", header_value)


def format_event(event: dict) -> str:
    """Format a single webhook event for display."""
    timestamp = event.get("timestamp", "")
    event_name = event.get("event", "unknown")
    sandbox_id = event.get("sandbox_id", "?")
    template_id = event.get("template_id", "?")
    parts = [f"  [{timestamp}] {event_name}"]
    parts.append(f"    sandbox_id : {sandbox_id}")
    if event.get("template_id"):
        parts.append(f"    template_id: {template_id}")
    # show extra fields beyond the known keys
    known = {"timestamp", "level", "event", "sandbox_id", "template_id"}
    for key, value in event.items():
        if key not in known:
            parts.append(f"    {key}: {json.dumps(value)}")
    return "\n".join(parts)


def json_response(handler: http.server.BaseHTTPRequestHandler,
                  status: int, body: dict):
    """Send a JSON response with proper Content-Type."""
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.end_headers()
    handler.wfile.write(json.dumps(body).encode() + b"\n")


class WebhookHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/webhook":
            json_response(self, 404, {"error": "not found"})
            return

        content_length = int(self.headers.get("Content-Length", "0"))
        raw_body = self.rfile.read(content_length) if content_length > 0 else b""

        signature = self.headers.get(SIGNATURE_HEADER)
        if not verify_hmac(raw_body, signature):
            json_response(self, 403, {"error": "HMAC signature mismatch"})
            return

        now = datetime.now().astimezone().isoformat(timespec="seconds")
        body_str = raw_body.decode(errors="replace")
        try:
            payload = json.loads(body_str)
        except json.JSONDecodeError:
            print(f"[{now}] webhook received non-JSON body")
            json_response(self, 400, {"error": "invalid JSON"})
            return

        events = payload.get("events")
        if events is None:
            json_response(self, 400, {"error": "missing 'events' field"})
            return
        if isinstance(events, dict):
            events = [events]
        if not isinstance(events, list):
            json_response(self, 400,
                          {"error": "'events' must be a list or object"})
            return

        print(f"[{now}] received {len(events)} event(s)")
        for event in events:
            if isinstance(event, dict):
                print(format_event(event))
            else:
                print(f"  [skipped] non-object event: {json.dumps(event)}")
        print(flush=True)

        json_response(self, 200, {"status": "ok"})

    def do_GET(self):
        if self.path == "/health":
            json_response(self, 200, {"status": "ok"})
        else:
            json_response(self, 404, {"error": "not found"})

    def log_message(self, format, *args):
        pass  # suppress default stderr access log


def main():
    mode = "without HMAC verification" if not HMAC_SECRET else "with HMAC verification"
    print(f"webhook receiver listening on :{PORT} {mode}")
    server = http.server.HTTPServer(("0.0.0.0", PORT), WebhookHandler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nshutting down")
        server.server_close()


if __name__ == "__main__":
    main()
