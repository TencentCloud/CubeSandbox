#!/usr/bin/env python3
"""
Cube Sandbox Webhook Receiver
=============================
A simple HTTP server that receives and displays sandbox lifecycle events
from CubeAPI via webhook callbacks.

Usage:
    # Basic (no signature verification)
    python receiver.py

    # With signature verification
    WEBHOOK_SECRET=my-secret python receiver.py

    # Custom host/port/path
    python receiver.py --host 0.0.0.0 --port 9999 --path /events

Then configure CubeAPI to send webhooks to this receiver.
"""

import argparse
import hmac
import json
import logging
import os
import sys
from datetime import datetime
from http import HTTPStatus
from http.server import HTTPServer, BaseHTTPRequestHandler

# ---------------------------------------------------------------------------
# Formatting helpers
# ---------------------------------------------------------------------------

# ANSI colour codes — disabled when stdout is not a terminal.
_USE_COLOR = hasattr(sys.stdout, "isatty") and sys.stdout.isatty()


def _c(code: str, text: str) -> str:
    return f"\033[{code}m{text}\033[0m" if _USE_COLOR else text


def bold(s: str) -> str:
    return _c("1", s)


def dim(s: str) -> str:
    return _c("2", s)


def green(s: str) -> str:
    return _c("32", s)


def yellow(s: str) -> str:
    return _c("33", s)


def cyan(s: str) -> str:
    return _c("36", s)


def red(s: str) -> str:
    return _c("31", s)


def magenta(s: str) -> str:
    return _c("35", s)

# ─── Event-type → colour mapping ─────────────────────────────────────────

EVENT_COLORS = {
    "sandbox.created": green,
    "sandbox.deleted": red,
    "sandbox.paused": yellow,
    "sandbox.resumed": green,
    "sandbox.timeout.updated": cyan,
    "sandbox.refreshed": cyan,
    "api.response": dim,
    "api.error": red,
}

DEFAULT_COLOR = magenta


def _format_timestamp(ts_str: str) -> str:
    """Pretty-print an ISO 8601 timestamp in the local time zone."""
    try:
        dt = datetime.fromisoformat(ts_str.replace("Z", "+00:00"))
        return dt.astimezone().strftime("%Y-%m-%d %H:%M:%S.%f")[:23]
    except (ValueError, TypeError):
        return ts_str


def display_event(event_name: str, body: dict) -> None:
    """Print one webhook event to stdout in a human-readable format."""
    color = EVENT_COLORS.get(event_name, DEFAULT_COLOR)
    label = color(event_name)

    ts = body.get("timestamp", "")
    time_str = _format_timestamp(ts)

    # Header line
    sandbox_id = body.get("sandbox_id") or "-"
    print(f"\n{label}  {time_str}  sandbox={dim(sandbox_id)}")

    # Fields (exclude known structural fields)
    skip = {"event", "timestamp", "sandbox_id", "template_id"}
    for key, value in body.items():
        if key in skip:
            continue
        if key == "fields" and isinstance(value, dict):
            # Sub-fields from the flattened `fields` map
            for sk, sv in value.items():
                if sk not in skip:
                    print(f"  {dim(sk)}: {sv}")
        else:
            print(f"  {dim(key)}: {value}")

    sys.stdout.flush()


# ---------------------------------------------------------------------------
# Request handler
# ---------------------------------------------------------------------------

class WebhookHandler(BaseHTTPRequestHandler):
    """Handles POST /<webhook-path> from CubeAPI."""

    # Shared across instances — set once at startup.
    webhook_path: str = "/webhook"
    secret: str | None = None

    # Silence default request-line logging (we log in a prettier way).
    def log_message(self, fmt, *args):
        return

    def _verify_signature(self, body: bytes) -> bool:
        """Verify X-Cube-Signature-256 header against HMAC-SHA256 of body."""
        if not self.secret:
            return True  # no secret configured → skip verification

        header = self.headers.get("X-Cube-Signature-256", "")
        if not header.startswith("sha256="):
            return False

        expected_sig = header[len("sha256="):]
        computed = hmac.new(
            self.secret.encode("utf-8"), body, "sha256"
        ).hexdigest()

        # Constant-time comparison to prevent timing attacks.
        return hmac.compare_digest(computed, expected_sig)

    def _reply(self, status: int, msg: str):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"status": msg}).encode("utf-8"))

    def do_POST(self):
        if self.path != self.webhook_path:
            self._reply(HTTPStatus.NOT_FOUND, "not found")
            return

        # Read body
        length = int(self.headers.get("Content-Length", 0))
        if length == 0:
            self._reply(HTTPStatus.BAD_REQUEST, "empty body")
            return
        body = self.rfile.read(length)

        # Verify HMAC signature
        if not self._verify_signature(body):
            self._reply(HTTPStatus.UNAUTHORIZED, "invalid signature")
            return

        # Parse JSON
        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self._reply(HTTPStatus.BAD_REQUEST, "invalid JSON")
            return

        event_name = payload.get("event", "unknown")

        # Acknowledge quickly
        self._reply(HTTPStatus.OK, "ok")

        # Display
        display_event(event_name, payload)

    # ── Health check (optional) ──────────────────────────────────────────
    def do_GET(self):
        if self.path == "/health":
            self._reply(HTTPStatus.OK, "alive")
        else:
            self._reply(HTTPStatus.NOT_FOUND, "not found")

    # Suppress noisy tracebacks for broken pipe / connection reset.
    def handle_one_request(self):
        try:
            super().handle_one_request()
        except (ConnectionResetError, BrokenPipeError):
            pass


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Cube Sandbox Webhook Receiver",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Examples:\n"
            "  python receiver.py\n"
            "  python receiver.py --port 9999 --path /events\n"
            "  WEBHOOK_SECRET=my-secret python receiver.py\n"
        ),
    )
    parser.add_argument(
        "--host", default="0.0.0.0",
        help="Listen address (default: 0.0.0.0)",
    )
    parser.add_argument(
        "--port", type=int, default=8081,
        help="Listen port (default: 8081)",
    )
    parser.add_argument(
        "--path", default="/webhook",
        help="Webhook URL path (default: /webhook)",
    )
    return parser.parse_args(argv)


def main() -> None:
    args = parse_args()

    # Pick up secret from environment (or fall back to command-line --secret).
    secret = os.environ.get("WEBHOOK_SECRET") or None

    # Patch the handler class.
    WebhookHandler.webhook_path = args.path
    WebhookHandler.secret = secret

    # Logging
    logging.basicConfig(level=logging.WARNING, format="%(message)s")

    server = HTTPServer((args.host, args.port), WebhookHandler)

    print(f"\n{bold(' Cube Sandbox Webhook Receiver '):─^50}")
    print(f"  URL:     http://{args.host}:{args.port}{args.path}")
    print(f"  Secret:  {'✓ enabled' if secret else '○ disabled (no signature check)'}")
    print(f"{'─' * 50}")
    print(f"  Listening for events…  (Ctrl+C to stop)\n")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print(f"\n{ dim('Shutting down…') }")
        server.shutdown()


if __name__ == "__main__":
    main()