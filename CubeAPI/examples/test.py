# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import time
import logging
import traceback
import threading
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

logging.basicConfig(
    level=logging.INFO,
    handlers=[
        logging.FileHandler("log.txt", encoding="utf-8"),
        logging.StreamHandler(),
    ],
)

TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]

PYTHON_CODE = """
print("hello cube")
"""


def get_log(worker_id):
    """Return a LoggerAdapter that injects worker_id into every log line."""
    return logging.LoggerAdapter(
        logging.getLogger(__name__),
        {"worker_id": worker_id},
    )


def run_once(worker_id):
    log = get_log(worker_id)
    log.info("=== loop start ===")

    with Sandbox.create(template=TEMPLATE_ID) as sandbox:
        log.info("sandbox created: %s", sandbox.sandbox_id)

        # exec python code
        try:
            result = sandbox.run_code(
                PYTHON_CODE,
                on_stdout=lambda data: log.info("[run_code stdout] %s", data),
            )
            log.info("run_code result: %s", result)
        except Exception:
            log.error("run_code failed:\n%s", traceback.format_exc())
        
        # exec shell cmd
        try:
            result = sandbox.commands.run("ls -l /")
            log.info("cmd stdout: %s", result.stdout.strip())
        except Exception:
            log.error("commands.run failed:\n%s", traceback.format_exc())
        
        try:
            file_content = sandbox.files.read("/etc/hosts")
            print(file_content)
        except Exception:
            log.error("commands.read failed:\n%s", traceback.format_exc())
    log.info("sandbox destroyed")


def worker_loop(worker_id):
    log = get_log(worker_id)
    log.info("worker started")
    while True:
        try:
            run_once(worker_id)
        except Exception:
            log.error("run_once failed:\n%s", traceback.format_exc())
        time.sleep(30)


def main():
    num_workers = 4
    threads = []
    for i in range(num_workers):
        t = threading.Thread(target=worker_loop, args=(i,), daemon=True)
        t.start()
        threads.append(t)

    # block main thread until all workers exit (they won't unless an unhandled signal occurs)
    for t in threads:
        t.join()


if __name__ == "__main__":
    main()