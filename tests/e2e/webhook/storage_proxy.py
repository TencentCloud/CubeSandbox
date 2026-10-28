#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Dedicated P6 S3 RPC fault fixture; forwards real storage calls unchanged.

Only rcow_create_snapshot and rcow_export_snapshot may be rejected; exports
may also receive an empty establishment reply. Successful snapshots and upload
states always come from the native backend.
"""
import argparse
import json
import os
from pathlib import Path
import socket
import sys
from socketserver import StreamRequestHandler, ThreadingUnixStreamServer
import threading
import time


sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk_compat"))
from framework.webhook_resources import owned_path

def serve(upstream, listen, control, evidence):
    upstream, listen, control = map(owned_path, (upstream, listen, control))
    lock = threading.Lock()
    class Server(ThreadingUnixStreamServer):
        daemon_threads = True
        slots = threading.BoundedSemaphore(4)
        def process_request(self, request, client_address):
            self.slots.acquire()
            try:
                super().process_request(request, client_address)
            except BaseException:
                self.slots.release()
                raise
        def process_request_thread(self, request, client_address):
            try:
                super().process_request_thread(request, client_address)
            finally:
                self.slots.release()

    class Handler(StreamRequestHandler):
        def handle(self):
            self.connection.settimeout(60)
            with socket.socket(socket.AF_UNIX) as remote:
                remote.settimeout(60)
                remote.connect(str(upstream))
                with remote.makefile("rb") as reader:
                    while True:
                        try:
                            line = self.rfile.readline(1024 * 1024)
                        except TimeoutError:
                            return
                        if not line:
                            return
                        assert line.endswith(b"\n"), "RPC frame exceeds fixture limit"
                        request = json.loads(line)
                        mode = control.read_text().strip()
                        reject_snapshot = request.get("method") == "rcow_create_snapshot" and mode == "snapshot-error"
                        reject_export = request.get("method") == "rcow_export_snapshot" and mode in {"error", "empty"}
                        if reject_snapshot or reject_export:
                            response = {"jsonrpc": "2.0", "id": request["id"]}
                            if mode != "empty":
                                response["error"] = {"code": -32000, "message": "P6 dedicated storage operation rejected"}
                            else:
                                response["result"] = ""
                            with lock:
                                evidence.write(json.dumps({"at": time.time(), "mode": mode,
                                    "upstream_executed": False, "request": request, "response": response}) + "\n")
                                evidence.flush()
                                os.fsync(evidence.fileno())
                            self.wfile.write(json.dumps(response).encode() + b"\n")
                        else:
                            remote.sendall(line)
                            response = reader.readline(1024 * 1024)
                            assert response.endswith(b"\n"), "incomplete upstream response"
                            self.wfile.write(response)
                        self.wfile.flush()

    with Server(str(listen), Handler) as server:
        print("listening " + str(listen), flush=True)
        server.serve_forever()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["upstream", "listen", "control", "evidence"]:
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    for value in (args.upstream, args.listen, args.control, args.evidence):
        owned_path(value)
    with args.evidence.open("x") as evidence:
        serve(args.upstream, args.listen, args.control, evidence)
