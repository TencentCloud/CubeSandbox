#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

import argparse
import hashlib
import hmac
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from os import environ


class WebhookHandler(BaseHTTPRequestHandler):
    secret = ""

    def do_POST(self):
        if self.path != "/webhook":
            self.send_error(404, "not found")
            return

        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        signature = self.headers.get("X-Cube-Signature-256", "")

        if self.secret and not verify_signature(self.secret, body, signature):
            self.send_error(401, "invalid signature")
            return

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "invalid json")
            return

        print(
            json.dumps(
                {
                    "event": payload.get("event"),
                    "timestamp": payload.get("timestamp"),
                    "sandbox_id": payload.get("sandbox_id"),
                    "template_id": payload.get("template_id"),
                    "headers": {
                        "X-Cube-Event": self.headers.get("X-Cube-Event"),
                        "X-Cube-Timestamp": self.headers.get("X-Cube-Timestamp"),
                        "X-Cube-Signature-256": signature,
                    },
                },
                ensure_ascii=False,
            ),
            flush=True,
        )

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok\n")

    def log_message(self, fmt, *args):
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)


def verify_signature(secret, body, signature):
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


def main():
    parser = argparse.ArgumentParser(description="CubeSandbox webhook receiver")
    parser.add_argument("--host", default=environ.get("WEBHOOK_HOST", "127.0.0.1"))
    parser.add_argument(
        "--port", type=int, default=int(environ.get("WEBHOOK_PORT", "9000"))
    )
    parser.add_argument("--secret", default=environ.get("WEBHOOK_SECRET", ""))
    args = parser.parse_args()

    WebhookHandler.secret = args.secret
    server = ThreadingHTTPServer((args.host, args.port), WebhookHandler)
    print(f"listening on http://{args.host}:{args.port}/webhook", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
