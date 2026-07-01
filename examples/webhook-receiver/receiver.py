#!/usr/bin/env python3
import hashlib
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer


SECRET = os.environ.get("WEBHOOK_SECRET", "")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if SECRET and not self.verify_signature(body):
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"invalid signature\n")
            return

        try:
            payload = json.loads(body.decode("utf-8"))
        except json.JSONDecodeError:
            payload = {"raw": body.decode("utf-8", errors="replace")}

        print(
            json.dumps(
                {
                    "path": self.path,
                    "event": self.headers.get("X-Cube-Event"),
                    "payload": payload,
                },
                ensure_ascii=False,
            ),
            flush=True,
        )
        self.send_response(204)
        self.end_headers()

    def verify_signature(self, body):
        timestamp = self.headers.get("X-Cube-Timestamp", "")
        signature = self.headers.get("X-Cube-Signature", "")
        signed = timestamp.encode("utf-8") + b"." + body
        expected = "sha256=" + hmac.new(
            SECRET.encode("utf-8"), signed, hashlib.sha256
        ).hexdigest()
        return hmac.compare_digest(signature, expected)


if __name__ == "__main__":
    host = os.environ.get("WEBHOOK_HOST", "127.0.0.1")
    port = int(os.environ.get("WEBHOOK_PORT", "9000"))
    print(f"listening on http://{host}:{port}/webhook", flush=True)
    HTTPServer((host, port), Handler).serve_forever()
