# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Real Unix RPC forwarding from a fresh, non-developer-specific run path."""
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest


class StorageProxyTest(unittest.TestCase):
    def test_forward_and_reject_in_custom_root(self):
        with tempfile.TemporaryDirectory(prefix="wh-rpc-") as directory:
            root = Path(directory)
            upstream, listener = root / "native.sock", root / "proxy.sock"
            control, evidence = root / "mode", root / "evidence.jsonl"
            control.write_text("forward")
            received = []
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(str(upstream))
                server.listen()
                server.settimeout(5)
                def native():
                    peer, _ = server.accept()
                    with peer, peer.makefile("rb") as stream:
                        line = stream.readline()
                        received.append(json.loads(line))
                        peer.sendall(b'{"jsonrpc":"2.0","id":1,"result":"native-result"}\n')
                        # Rejected request should never reach this connection.
                        extra = stream.readline()
                        if extra:
                            received.append(json.loads(extra))
                thread = threading.Thread(target=native, daemon=True)
                thread.start()
                process = subprocess.Popen([sys.executable, str(Path(__file__).with_name("storage_proxy.py")),
                    "--upstream", str(upstream), "--listen", str(listener),
                    "--control", str(control), "--evidence", str(evidence)],
                    env={**os.environ, "WEBHOOK_P6_RUN_ROOT": str(root)},
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    deadline = time.monotonic() + 5
                    while not listener.exists() and time.monotonic() < deadline and process.poll() is None:
                        time.sleep(.02)
                    self.assertTrue(listener.exists())
                    with socket.socket(socket.AF_UNIX) as client:
                        client.settimeout(5)
                        client.connect(str(listener))
                        with client.makefile("rb") as response:
                            client.sendall(b'{"id":1,"method":"rcow_create_snapshot"}\n')
                            self.assertEqual(json.loads(response.readline())["result"], "native-result")
                            control.write_text("snapshot-error")
                            client.sendall(b'{"id":2,"method":"rcow_create_snapshot"}\n')
                            self.assertIn("error", json.loads(response.readline()))
                    thread.join(5)
                    self.assertFalse(thread.is_alive())
                    self.assertEqual(len(received), 1)
                    rows = [json.loads(line) for line in evidence.read_text().splitlines()]
                    self.assertFalse(rows[0]["upstream_executed"])
                finally:
                    process.terminate()
                    process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
