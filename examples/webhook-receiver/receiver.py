#!/usr/bin/env python3
import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


HOST = os.getenv("WEBHOOK_RECEIVER_HOST", "0.0.0.0")
PORT = int(os.getenv("WEBHOOK_RECEIVER_PORT", "8088"))
SECRET = os.getenv("CUBE_WEBHOOK_SECRET_0", "")


class WebhookHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)

        if SECRET and not self._signature_valid(body):
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"invalid signature\n")
            return

        try:
            payload = json.loads(body.decode("utf-8"))
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"invalid json\n")
            return

        self.send_response(204)
        self.end_headers()
        print(json.dumps(payload, ensure_ascii=False, indent=2), flush=True)

    def _signature_valid(self, body):
        received = self.headers.get("X-Cube-Signature-256", "")
        expected = "sha256=" + hmac.new(
            SECRET.encode("utf-8"), body, hashlib.sha256
        ).hexdigest()
        return hmac.compare_digest(received, expected)

    def log_message(self, fmt, *args):
        print("[access] " + fmt % args, flush=True)


def main():
    server = ThreadingHTTPServer((HOST, PORT), WebhookHandler)
    print(f"Listening on http://{HOST}:{PORT}/webhook", flush=True)
    if SECRET:
        print("HMAC verification enabled", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
