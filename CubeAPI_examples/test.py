# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
test.py — Concurrent multi-worker smoke test: create → run_code → cmd → read → destroy.

Original used: e2b_code_interpreter with sandbox.commands.run() + sandbox.files.read()
Ported to: cubesandbox v0.1.0 using run_code() for all data-plane operations.

Data-plane stream (HTTP:80) is exercised for each run_code call.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
import time
import logging
import traceback
import threading
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [worker-%(worker_id)s] %(message)s",
    handlers=[
        logging.FileHandler("CubeAPI_examples/log.txt", encoding="utf-8"),
        logging.StreamHandler(),
    ],
)

# Suppress httpx internal logs that don't carry worker_id
logging.getLogger("httpx").setLevel(logging.WARNING)
logging.getLogger("httpcore").setLevel(logging.WARNING)

TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]

PYTHON_CODE = """
print("hello cube")
"""


def get_log(worker_id):
    return logging.LoggerAdapter(
        logging.getLogger(__name__),
        {"worker_id": worker_id},
    )


def run_once(worker_id):
    log = get_log(worker_id)
    log.info("=== loop start ===")

    with Sandbox.create(template=TEMPLATE_ID) as sandbox:
        log.info("sandbox created: %s", sandbox.sandbox_id)

        # exec python code (data-plane HTTP:80 stream)
        try:
            result = sandbox.run_code(
                PYTHON_CODE,
                on_stdout=lambda data: log.info("[run_code stdout] %s", data.text.rstrip()),
            )
            log.info("run_code result: %s", result)
        except Exception:
            log.error("run_code failed:\n%s", traceback.format_exc())

        # exec shell cmd via subprocess (data-plane HTTP:80 stream)
        try:
            result = sandbox.run_code(
                "import subprocess; print(subprocess.check_output(['ls','-l','/'], text=True))"
            )
            stdout = result.logs.stdout[0].strip() if result.logs.stdout else (result.text or "")
            log.info("cmd stdout (first 120 chars): %s", stdout[:120])
        except Exception:
            log.error("commands.run failed:\n%s", traceback.format_exc())

        # read file (data-plane HTTP:80 stream)
        try:
            result = sandbox.run_code("print(open('/etc/hosts').read())")
            content = result.logs.stdout[0] if result.logs.stdout else (result.text or "")
            log.info("files.read /etc/hosts: %s", content[:80].replace("\n", "\\n"))
        except Exception:
            log.error("files.read failed:\n%s", traceback.format_exc())

    log.info("sandbox destroyed")


def worker_loop(worker_id, iterations=2):
    """Run `iterations` times then exit (instead of infinite loop for test purposes)."""
    log = get_log(worker_id)
    log.info("worker started")
    for _ in range(iterations):
        try:
            run_once(worker_id)
        except Exception:
            log.error("run_once failed:\n%s", traceback.format_exc())
        time.sleep(1)
    log.info("worker done")


def main():
    num_workers = 4
    iterations = 1   # 1 iteration per worker for quick smoke test
    threads = []
    for i in range(num_workers):
        t = threading.Thread(target=worker_loop, args=(i, iterations), daemon=False)
        t.start()
        threads.append(t)

    for t in threads:
        t.join()

    print("\nAll workers finished. See CubeAPI_examples/log.txt for details.")


if __name__ == "__main__":
    main()
