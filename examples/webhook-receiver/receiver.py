#!/usr/bin/env python3
# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def expected_signature(secret: str, timestamp: str, nonce: str, body: bytes) -> str:
    message = timestamp.encode() + b"." + nonce.encode() + b"." + body
    digest = hmac.new(secret.encode(), message, hashlib.sha256).hexdigest()
    return "sha256=" + digest


class WebhookHandler(BaseHTTPRequestHandler):
    server_version = "CubeWebhookReceiver/1.0"

    def do_POST(self) -> None:
        if self.path != "/webhook":
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b"not found\n")
            return

        length = int(self.headers.get("content-length", "0"))
        body = self.rfile.read(length)
        secret = os.environ.get("WEBHOOK_SECRET", "")

        if secret:
            timestamp = self.headers.get("X-Cube-Timestamp", "")
            nonce = self.headers.get("X-Cube-Nonce", "")
            signature = self.headers.get("X-Cube-Signature-256", "")
            expected = expected_signature(secret, timestamp, nonce, body)
            if not hmac.compare_digest(signature, expected):
                self.send_response(401)
                self.end_headers()
                self.wfile.write(b"invalid signature\n")
                print("signature=invalid")
                return
            print("signature=valid")

        try:
            payload = json.loads(body.decode("utf-8"))
        except json.JSONDecodeError:
            payload = {"raw": body.decode("utf-8", errors="replace")}

        print(
            json.dumps(
                {
                    "event": self.headers.get("X-Cube-Event"),
                    "delivery": self.headers.get("X-Cube-Delivery"),
                    "payload": payload,
                },
                ensure_ascii=False,
                sort_keys=True,
            ),
            flush=True,
        )

        self.send_response(204)
        self.end_headers()

    def log_message(self, fmt: str, *args) -> None:
        print("%s - %s" % (self.address_string(), fmt % args))


def main() -> None:
    host = os.environ.get("WEBHOOK_HOST", "127.0.0.1")
    port = int(os.environ.get("WEBHOOK_PORT", "9000"))
    server = ThreadingHTTPServer((host, port), WebhookHandler)
    print(f"listening on http://{host}:{port}/webhook")
    server.serve_forever()


if __name__ == "__main__":
    main()
