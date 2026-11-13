# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
import ast
import base64
import http.client
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest


class ReceiverTest(unittest.TestCase):
    def test_required_signature_and_strict_execution_reject_actual_missing_fields(self):
        with tempfile.TemporaryDirectory() as directory:
            output, mode = Path(directory) / "requests.jsonl", Path(directory) / "mode"
            mode.write_text("require-signature")
            process = subprocess.Popen([
                sys.executable, str(Path(__file__).with_name("receiver.py")),
                "--port", "0", "--output", str(output), "--mode-file", str(mode), "--fsync",
            ], stdout=subprocess.PIPE, text=True)
            try:
                address = ast.literal_eval(process.stdout.readline().removeprefix("listening "))
                def post(payload, headers=None):
                    conn = http.client.HTTPConnection(*address, timeout=3)
                    try:
                        conn.request("POST", "/", json.dumps(payload), headers or {})
                        return conn.getresponse().status
                    finally:
                        conn.close()
                payload = {"id": "test", "sandbox_execution_id": "", "event_data": {}}
                self.assertEqual(post(payload), 401)
                # Presence alone must never be mistaken for a verified signature.
                self.assertEqual(post(payload, {"e2b-signature": "unverified", "e2b-signature-version": "v1"}), 501)
                mode.write_text("strict-execution")
                self.assertEqual(post(payload), 422)
                payload["sandbox_execution_id"] = "11111111-1111-4111-8111-111111111111"
                payload["event_data"]["execution"] = {"started_at": "", "execution_time": 1}
                self.assertEqual(post(payload), 422)
                payload["event_data"]["execution"]["started_at"] = "2026-10-24T00:00:00Z"
                self.assertEqual(post(payload), 204)
                rows = [json.loads(line) for line in output.read_text().splitlines()]
                self.assertEqual([row["rejection_reason"] for row in rows], [
                    "signature_headers_missing", "signature_verifier_not_configured",
                    "execution_id_not_uuid", "execution_time_or_start_missing", None])
            finally:
                process.terminate()
                process.wait(timeout=5)
                process.stdout.close()

    def test_record_before_drop_status_change_and_capacity(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "requests.jsonl"
            mode = Path(directory) / "mode"
            mode.write_text("503")
            process = subprocess.Popen([
                sys.executable, str(Path(__file__).with_name("receiver.py")),
                "--port", "0", "--output", str(output), "--mode-file", str(mode),
                "--limit", "5", "--fsync",
            ], stdout=subprocess.PIPE, text=True)
            try:
                address = ast.literal_eval(process.stdout.readline().removeprefix("listening "))
                body = b'{ "id":"event", "type":"sandbox.lifecycle.created", "sandbox_id":"test" }'
                def post(delivery, payload=body):
                    conn = http.client.HTTPConnection(*address, timeout=3)
                    try:
                        conn.request("POST", "/", payload, {"Content-Type": "application/json", "e2b-webhook-id": "sub", "e2b-delivery-id": delivery})
                        return conn.getresponse().status
                    finally:
                        conn.close()
                self.assertEqual(post("first"), 503)
                mode.write_text("drop")
                with self.assertRaises(http.client.RemoteDisconnected):
                    post("second")
                mode.write_text("retry-once")
                self.assertEqual(post("third"), 204)
                fresh = body.replace(b'"event"', b'"new-event"')
                self.assertEqual(post("fourth", fresh), 503)
                self.assertEqual(post("fifth", fresh), 204)
                self.assertEqual(post("over-limit"), 507)
                records = [json.loads(line) for line in output.read_text().splitlines()]
                self.assertEqual([r["mode"] for r in records], ["503", "drop", "retry-once", "retry-once", "retry-once"])
                self.assertEqual([r["delivery_id"] for r in records], ["first", "second", "third", "fourth", "fifth"])
                self.assertEqual([r["response_status"] for r in records[-2:]], [503, 204])
                self.assertEqual(records[-1]["response_status"], 204)
                self.assertFalse(records[-1]["signature_present"])
                for index, record in enumerate(records):
                    self.assertEqual(base64.b64decode(record["body_base64"]), body if index < 3 else fresh)
                conn = http.client.HTTPConnection(*address, timeout=3)
                try:
                    conn.request("POST", "/", b"", {"Content-Length": str(256*1024+1)})
                    self.assertEqual(conn.getresponse().status, 413)
                finally:
                    conn.close()
            finally:
                process.terminate()
                process.wait(timeout=5)
                process.stdout.close()

    def test_parallel_handler_bound_and_shared_record_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "requests.jsonl"
            mode = Path(directory) / "mode"
            mode.write_text("slow")
            process = subprocess.Popen([
                sys.executable, str(Path(__file__).with_name("receiver.py")),
                "--port", "0", "--output", str(output), "--mode-file", str(mode),
                "--workers", "2", "--slow-seconds", "1", "--limit", "3",
            ], stdout=subprocess.PIPE, text=True)
            results = []
            threads = []
            try:
                address = ast.literal_eval(process.stdout.readline().removeprefix("listening "))

                def post(number):
                    conn = http.client.HTTPConnection(*address, timeout=4)
                    try:
                        conn.request("POST", "/", json.dumps({"id": str(number)}))
                        results.append(conn.getresponse().status)
                    finally:
                        conn.close()

                for number in range(2):
                    thread = threading.Thread(target=post, args=(number,))
                    thread.start()
                    threads.append(thread)
                deadline = time.monotonic() + 3
                while len(output.read_text().splitlines()) < 2 and time.monotonic() < deadline:
                    time.sleep(0.01)
                rows = [json.loads(line) for line in output.read_text().splitlines()]
                self.assertEqual(len(rows), 2)
                self.assertEqual(max(row["active_requests"] for row in rows), 2)
                mode.write_text("204")
                thread = threading.Thread(target=post, args=(2,))
                thread.start()
                threads.append(thread)
                time.sleep(0.1)
                self.assertEqual(len(output.read_text().splitlines()), 2, "third handler escaped bound")
                for thread in threads:
                    thread.join(timeout=4)
                    self.assertFalse(thread.is_alive())
                self.assertEqual(results, [204, 204, 204])
                post(3)
                self.assertEqual(results[-1], 507)
                self.assertEqual(len(output.read_text().splitlines()), 3)
            finally:
                process.terminate()
                process.wait(timeout=5)
                process.stdout.close()
