#!/usr/bin/env python3
# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Minimal CubeSandbox webhook receiver.

A dependency-free (Python standard library only) HTTP server that receives
CubeSandbox webhook events, optionally verifies the HMAC-SHA256 signature, and
pretty-prints each event to stdout.

Usage:
    # No signature verification
    python3 receiver.py

    # Verify signatures against a shared secret (must match the `secret`
    # configured for this endpoint in the CubeAPI webhook config file)
    CUBE_WEBHOOK_SECRET=my-shared-secret python3 receiver.py

    # Listen on a different address / port (default: 0.0.0.0:9100)
    HOST=0.0.0.0 PORT=9100 python3 receiver.py

Point a CubeAPI webhook endpoint at http://<this-host>:9100/webhook and
create / pause / resume / delete a sandbox to see events arrive here.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import sys
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOST = os.environ.get("HOST", "0.0.0.0")
PORT = int(os.environ.get("PORT", "9100"))
SECRET = os.environ.get("CUBE_WEBHOOK_SECRET", "")


def verify_signature(secret: str, body: bytes, header: str | None) -> bool:
    """Return True if `header` is a valid `sha256=<hex>` HMAC of `body`.

    Uses a constant-time comparison to avoid timing attacks.
    """
    if not header or not header.startswith("sha256="):
        return False
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    received = header[len("sha256=") :]
    return hmac.compare_digest(expected, received)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        event = self.headers.get("X-Cube-Event", "?")
        delivery = self.headers.get("X-Cube-Delivery", "?")
        signature = self.headers.get("X-Cube-Signature")

        # Verify the signature when a secret is configured.
        if SECRET:
            if not verify_signature(SECRET, body, signature):
                print(f"[{_now()}] ✗ REJECTED {event}: bad or missing signature")
                self.send_response(401)
                self.end_headers()
                self.wfile.write(b"invalid signature")
                return
            verified = "verified ✓"
        else:
            verified = "not checked (no secret set)"

        try:
            payload = json.loads(body)
            pretty = json.dumps(payload, indent=2, ensure_ascii=False)
        except json.JSONDecodeError:
            pretty = body.decode("utf-8", "replace")

        print(f"[{_now()}] ✓ {event}  delivery={delivery}  (signature {verified})")
        print(pretty)
        print("-" * 60)

        # Always ACK quickly with 2xx so CubeAPI marks delivery as successful.
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *_args) -> None:  # silence default access logging
        pass


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")


def main() -> None:
    mode = "signature verification ON" if SECRET else "signature verification OFF"
    print(f"CubeSandbox webhook receiver listening on http://{HOST}:{PORT}/webhook")
    print(f"  {mode}")
    print("  waiting for events (Ctrl+C to stop) ...")
    print("-" * 60)
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nshutting down")
        server.shutdown()
        sys.exit(0)


if __name__ == "__main__":
    main()
