#!/usr/bin/env python3
"""Minimal CubeMaster contract mock for local CubeAPI webhook verification."""

import argparse
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse


def success(request_id=""):
    return {"RequestID": request_id, "ret": {"ret_code": 0, "ret_msg": "success"}}


class Handler(BaseHTTPRequestHandler):
    sandbox_id = "mock-sandbox-1"
    template_id = "tpl-local"

    def respond(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def read_json(self):
        size = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(size) or b"{}")

    def do_POST(self):
        request = self.read_json()
        if self.path == "/cube/sandbox":
            self.template_id = request.get("annotations", {}).get(
                "cube.master.appsnapshot.template.id", self.template_id
            )
            payload = success(request.get("RequestID", request.get("requestID", "")))
            payload.update({"sandbox_id": self.sandbox_id, "ext_info": {}})
            return self.respond(payload)
        if self.path == "/cube/sandbox/update":
            return self.respond(success(request.get("requestID", "")))
        self.send_error(404)

    def do_DELETE(self):
        request = self.read_json()
        if self.path == "/cube/sandbox":
            payload = success(request.get("RequestID", request.get("requestID", "")))
            payload["sandbox_id"] = request.get("sandbox_id", self.sandbox_id)
            return self.respond(payload)
        self.send_error(404)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/cube/sandbox/info":
            sandbox_id = parse_qs(parsed.query).get("sandbox_id", [self.sandbox_id])[0]
            payload = success()
            payload["data"] = [{
                "sandbox_id": sandbox_id,
                "status": 1,
                "host_id": "mock-host",
                "template_id": self.template_id,
                "annotations": {},
                "labels": {},
                "containers": [],
            }]
            return self.respond(payload)
        self.send_error(404)

    def log_message(self, fmt, *args):
        print(fmt % args, flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=18089)
    args = parser.parse_args()
    HTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
