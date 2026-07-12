#!/usr/bin/env python3
# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import argparse
import hashlib
import hmac
import json
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer


class Receiver(BaseHTTPRequestHandler):
    secret = None
    tolerance_seconds = 300
    wechat_webhook_url = None

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.secret and not self.verify_signature(body):
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"invalid signature\n")
            return

        payload = json.loads(body.decode("utf-8"))
        print(json.dumps(payload, indent=2, sort_keys=True), flush=True)
        if self.wechat_webhook_url:
            self.forward_to_wechat(payload)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok\n")

    def do_GET(self):
        if self.path != "/health":
            self.send_error(404)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok\n")

    def log_message(self, fmt, *args):
        return

    def forward_to_wechat(self, payload):
        text = (
            f"CubeSandbox webhook: {payload.get('event')}\n"
            f"sandbox_id: {payload.get('sandbox_id')}\n"
            f"template_id: {payload.get('template_id', '')}\n"
            f"timestamp: {payload.get('timestamp')}\n"
            f"event_id: {payload.get('event_id')}"
        )
        data = json.dumps({"msgtype": "text", "text": {"content": text}}).encode("utf-8")
        req = urllib.request.Request(
            self.wechat_webhook_url,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(req, timeout=3).read()
        except urllib.error.URLError as exc:
            print(f"failed to forward to WeChat: {exc}", flush=True)

    def verify_signature(self, body):
        timestamp = self.headers.get("X-Cube-Webhook-Timestamp")
        signature = self.headers.get("X-Cube-Webhook-Signature")
        if not timestamp or not signature:
            return False
        try:
            ts = int(timestamp)
        except ValueError:
            return False
        if abs(int(time.time()) - ts) > self.tolerance_seconds:
            return False
        expected = hmac.new(
            self.secret.encode("utf-8"),
            timestamp.encode("utf-8") + b"." + body,
            hashlib.sha256,
        ).hexdigest()
        return hmac.compare_digest(signature, "sha256=" + expected)


def main():
    parser = argparse.ArgumentParser(description="CubeSandbox webhook receiver example")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9000)
    parser.add_argument("--secret", default=None)
    parser.add_argument("--tolerance-seconds", type=int, default=300)
    parser.add_argument("--wechat-webhook-url", default=None)
    args = parser.parse_args()

    Receiver.secret = args.secret
    Receiver.tolerance_seconds = args.tolerance_seconds
    Receiver.wechat_webhook_url = args.wechat_webhook_url
    server = HTTPServer((args.host, args.port), Receiver)
    print(f"listening on http://{args.host}:{args.port}/webhook", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
