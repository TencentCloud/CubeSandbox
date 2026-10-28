#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Bounded receiver for a dedicated P6 deployment.

Records raw bytes before replying. A mode file allows failure injection without
restarting the receiver: 204, 400, 401, 503, drop, slow (10s then 204), or retry-once (503 then 204 per event).
Never use sensitive payloads: this evidence file deliberately contains bodies.
"""
import argparse
import base64
import json
import os
import socket
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from socketserver import ThreadingMixIn
import threading
from uuid import UUID


def rejection(payload, headers, mode):
    if mode == "require-signature":
        if not headers.get("e2b-signature") or not headers.get("e2b-signature-version"):
            return 401, "signature_headers_missing"
        # This test receiver has no signing secret or cryptographic verifier.
        # Never accept arbitrary headers as authenticated requests.
        return 501, "signature_verifier_not_configured"
    if mode == "strict-execution":
        try:
            UUID(payload.get("sandbox_execution_id", ""))
        except (ValueError, TypeError, AttributeError):
            return 422, "execution_id_not_uuid"
        execution = payload.get("event_data", {}).get("execution", {})
        try:
            started = datetime.fromisoformat(execution.get("started_at", "").replace("Z", "+00:00"))
            assert started.tzinfo is not None and type(execution.get("execution_time")) is int
        except (ValueError, TypeError, AttributeError, AssertionError):
            return 422, "execution_time_or_start_missing"
    return 204, None


def serve(host, port, output, mode_file, limit, durable=False, workers=1, slow_seconds=10):
    attempts = {}
    lock = threading.Lock()

    class ConcurrentServer(ThreadingMixIn, HTTPServer):
        daemon_threads = True
        slots = threading.BoundedSemaphore(workers)

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

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            with lock:
                self.server.active += 1
            try:
                self.post()
            finally:
                with lock:
                    self.server.active -= 1

        def post(self):
            self.connection.settimeout(5)
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 256 * 1024:
                    self.send_error(413)
                    return
                body = self.rfile.read(length)
                if len(body) != length:
                    self.send_error(400)
                    return
                payload = json.loads(body)
                if not isinstance(payload, dict):
                    self.send_error(400)
                    return
                mode = mode_file.read_text().strip() if mode_file else "204"
                if mode not in {"204", "400", "401", "503", "drop", "slow", "retry-once", "require-signature", "strict-execution"}:
                    self.send_error(500)
                    return
                with lock:
                    if self.server.records >= limit:
                        self.send_error(507)
                        return
                    event_id = payload.get("id")
                    attempts[event_id] = attempts.get(event_id, 0) + 1
                    status = (503 if attempts[event_id] == 1 else 204) if mode == "retry-once" else (204 if mode == "slow" else None)
                    reason = None
                    if mode in {"require-signature", "strict-execution"}:
                        status, reason = rejection(payload, self.headers, mode)
                    output.write(json.dumps({
                        "active_requests": self.server.active,
                        "received_at": datetime.now(timezone.utc).isoformat(),
                        "body_base64": base64.b64encode(body).decode(),
                        "event_id": payload.get("id"),
                        "sandbox_id": payload.get("sandbox_id"),
                        "type": payload.get("type"),
                        "webhook_id": self.headers.get("e2b-webhook-id"),
                        "delivery_id": self.headers.get("e2b-delivery-id"),
                        "content_type": self.headers.get("Content-Type"),
                        "mode": mode,
                        "signature_present": "e2b-signature" in self.headers,
                        "signature_version_present": "e2b-signature-version" in self.headers,
                        "response_status": status if status is not None else (None if mode == "drop" else int(mode)),
                        "rejection_reason": reason,
                    }) + "\n")
                    output.flush()
                    if durable:
                        os.fsync(output.fileno())
                    self.server.records += 1
                if mode == "drop":
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.close_connection = True
                    return
                if mode == "slow":
                    time.sleep(slow_seconds)
                self.send_response(status if status is not None else int(mode))
                self.send_header("Content-Length", "0")
                self.end_headers()
            except (ValueError, OSError):
                self.close_connection = True

    with (HTTPServer if workers == 1 else ConcurrentServer)((host, port), Handler) as server:
        server.records = 0
        server.active = 0
        print(f"listening {server.server_address}", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18096)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--mode-file", type=Path)
    parser.add_argument("--fsync", action="store_true", help="persist each test record before replying")
    parser.add_argument("--limit", type=int, default=10000)
    parser.add_argument("--workers", type=int, default=1, help="bounded concurrent handlers (1–16)")
    parser.add_argument("--slow-seconds", type=float, default=10)
    args = parser.parse_args()
    if not 1 <= args.workers <= 16 or not 0 < args.slow_seconds <= 60:
        parser.error("workers must be 1–16 and slow-seconds must be >0 and <=60")
    if args.limit <= 0:
        parser.error("limit must be positive")
    # Exclusive creation avoids overwriting previous evidence.
    with args.output.open("x", encoding="utf-8") as output:
        serve(args.host, args.port, output, args.mode_file, args.limit, args.fsync, args.workers, args.slow_seconds)
