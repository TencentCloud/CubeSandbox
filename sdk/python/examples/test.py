# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import logging
import os
import sys
import threading
import time
import traceback

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

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
    return logging.LoggerAdapter(
        logging.getLogger(__name__),
        {"worker_id": worker_id},
    )


def run_once(worker_id):
    log = get_log(worker_id)
    log.info("=== loop start ===")

    with Sandbox.create(template=TEMPLATE_ID) as sandbox:
        log.info("sandbox created: %s", sandbox.sandbox_id)

        try:
            result = sandbox.run_code(
                PYTHON_CODE,
                on_stdout=lambda data: log.info("[run_code stdout] %s", data),
            )
            log.info("run_code result: %s", result)
        except Exception:
            log.error("run_code failed:\n%s", traceback.format_exc())

        try:
            result = sandbox.commands.run("ls -l /")
            log.info("cmd stdout: %s", result.stdout.strip())
        except Exception:
            log.error("commands.run failed:\n%s", traceback.format_exc())

    log.info("=== loop end ===")


NUM_WORKERS = 4
NUM_LOOPS = 3

threads = []
for i in range(NUM_WORKERS):
    t = threading.Thread(target=lambda wid=i: [run_once(wid) for _ in range(NUM_LOOPS)])
    threads.append(t)

for t in threads:
    t.start()

for t in threads:
    t.join()

print("done")
