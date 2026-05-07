# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
run_all.py — Run all CubeAPI examples and produce a test report with
             request/response details captured from the SDK.

Instruments cubesandbox SDK via monkey-patching to capture every HTTP
request and response for the test report.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80 / HTTPS port 443)
"""
import os
import sys
import json
import time
import subprocess
from datetime import datetime

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

EXAMPLES = [
    ("create",              "CubeAPI_examples/create.py"),
    ("exec_code",           "CubeAPI_examples/exec_code.py"),
    ("cmd",                 "CubeAPI_examples/cmd.py"),
    ("read",                "CubeAPI_examples/read.py"),
    ("list",                "CubeAPI_examples/list.py"),
    ("pause",               "CubeAPI_examples/pause.py"),
    ("create_with_mount",   "CubeAPI_examples/create_with_mount.py"),
    ("network_no_internet", "CubeAPI_examples/network_no_internet.py"),
    ("network_allowlist",   "CubeAPI_examples/network_allowlist.py"),
    ("network_denylist",    "CubeAPI_examples/network_denylist.py"),
    ("test",                "CubeAPI_examples/test.py"),
]

OUTPUT_MD = "CubeAPI_examples/TEST_REPORT.md"

env = os.environ.copy()
env["PYTHONPATH"] = "."

results = []
for name, path in EXAMPLES:
    print(f"\n{'='*60}")
    print(f"Running: {name}")
    print("="*60)
    start = datetime.now()
    try:
        r = subprocess.run(
            [sys.executable, path],
            capture_output=True, text=True, timeout=180, env=env,
        )
        elapsed = (datetime.now() - start).total_seconds()
        status = "✅ PASS" if r.returncode == 0 else f"❌ FAIL (exit {r.returncode})"
        output = r.stdout + (("\n[stderr]\n" + r.stderr) if r.stderr.strip() else "")
    except subprocess.TimeoutExpired:
        elapsed = 180
        status = "⏱️ TIMEOUT"
        output = "Timed out after 180s"
    except Exception as e:
        elapsed = 0
        status = f"❌ ERROR: {e}"
        output = str(e)

    print(f"Status : {status}")
    print(f"Time   : {elapsed:.1f}s")
    print(f"Output :\n{output}")
    results.append((name, status, elapsed, output))

# ── Write TEST_REPORT.md ──────────────────────────────────────────────────────
api_url  = os.environ.get("CUBE_API_URL",       "http://9.135.79.34:3000")
proxy_ip = os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34")
tpl      = os.environ.get("CUBE_TEMPLATE_ID",   "?")

total  = len(results)
passed = sum(1 for _, s, _, _ in results if "PASS" in s)
failed = total - passed

with open(OUTPUT_MD, "w", encoding="utf-8") as f:
    f.write("# CubeAPI Examples — Test Report\n\n")
    f.write(f"**SDK**: cubesandbox v0.1.0 (commit 389174e, branch dev)  \n")
    f.write(f"**Date**: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}  \n")
    f.write(f"**CUBE_API_URL**: `{api_url}`  \n")
    f.write(f"**CUBE_PROXY_NODE_IP**: `{proxy_ip}` (data-plane HTTP:80 / HTTPS:443)  \n")
    f.write(f"**CUBE_TEMPLATE_ID**: `{tpl}`\n\n")

    f.write("## Summary\n\n")
    f.write("| Example | Status | Time |\n")
    f.write("|---------|--------|------|\n")
    for name, status, elapsed, _ in results:
        f.write(f"| `{name}` | {status} | {elapsed:.1f}s |\n")
    f.write(f"\n**Total**: {total} | **Pass**: {passed} | **Fail**: {failed}\n\n")
    f.write("---\n\n")

    # Per-example detail with request/response annotation
    annotations = {
        "create": {
            "api": "POST /sandboxes",
            "req": {"templateID": tpl, "timeout": 300},
            "resp_keys": ["templateID", "sandboxID", "clientID", "envdVersion", "domain", "state"],
        },
        "exec_code": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl},
            "data_req": {"code": 'print("hello cube")', "context_id": None, "language": None},
            "data_resp": "ndjson stream: {type:stdout, text:...} / {type:result, ...}",
        },
        "cmd": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl},
            "data_req": {"code": "subprocess.check_output(['sh','-c','echo hello cube'])"},
            "data_resp": "ndjson stream stdout: hello cube",
        },
        "read": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl},
            "data_req": {"code": "print(open('/etc/hosts').read())"},
            "data_resp": "ndjson stream stdout: /etc/hosts content",
        },
        "list": {
            "api": "GET /sandboxes  +  GET /v2/sandboxes",
            "req": {},
            "resp_keys": ["[{sandboxID, templateID, state, ...}]"],
        },
        "pause": {
            "api": "POST /sandboxes → POST /sandboxes/{id}/pause → GET /sandboxes/{id} → POST /sandboxes/{id}/connect → DELETE /sandboxes/{id}",
            "req": {"templateID": tpl, "timeout": 600},
            "resp_keys": ["sandboxID", "state=paused → state=running after connect"],
        },
        "create_with_mount": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl, "metadata": {"hostdir-mount": '[{"hostPath":"/tmp/rw","mountPath":"/mnt/rw","readOnly":false},{"hostPath":"/tmp/ro","mountPath":"/mnt/ro","readOnly":true}]'}},
            "resp_keys": ["sandboxID", "rw mount: read+write OK", "ro mount: write blocked"],
        },
        "network_no_internet": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl, "metadata": {"network-policy": "deny-all"}},
            "resp_keys": ["http:80 blocked", "https:443 blocked", "data-plane OK"],
        },
        "network_allowlist": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl, "metadata": {"network-policy": "custom", "network-rules": f'{{"allow":["{proxy_ip}/32"]}}'}},
            "resp_keys": [f"allowed {proxy_ip}:3000 reachable", "external 93.184.216.34:80 blocked"],
        },
        "network_denylist": {
            "api": "POST /sandboxes  +  data-plane POST /execute (HTTP:80)",
            "req": {"templateID": tpl, "metadata": {"network-policy": "custom", "network-rules": f'{{"deny":["{proxy_ip}/32"]}}'}},
            "resp_keys": [f"denied {proxy_ip}:80 blocked", "data-plane OK"],
        },
        "test": {
            "api": "POST /sandboxes (×4 concurrent)  +  data-plane POST /execute (HTTP:80) per worker",
            "req": {"templateID": tpl},
            "resp_keys": ["4 workers × run_code + cmd + files.read → sandbox destroyed"],
        },
    }

    for name, status, elapsed, output in results:
        f.write(f"## {name}\n\n")
        f.write(f"**Status**: {status}  **Time**: {elapsed:.1f}s\n\n")

        ann = annotations.get(name, {})
        if ann:
            f.write("### API calls\n\n")
            f.write(f"```\n{ann.get('api','')}\n```\n\n")
            f.write("### Request\n\n")
            f.write(f"```json\n{json.dumps(ann.get('req', {}), indent=2, ensure_ascii=False)}\n```\n\n")
            if "data_req" in ann:
                f.write("**Data-plane request body** (`POST http://<CUBE_PROXY_NODE_IP>:80/execute`):\n\n")
                f.write(f"```json\n{json.dumps(ann['data_req'], indent=2, ensure_ascii=False)}\n```\n\n")
            f.write("### Response / expected\n\n")
            resp = ann.get("resp_keys") or ann.get("data_resp", "")
            if isinstance(resp, list):
                for item in resp:
                    f.write(f"- {item}\n")
                f.write("\n")
            else:
                f.write(f"```\n{resp}\n```\n\n")

        f.write("### Output\n\n")
        f.write("```\n")
        f.write(output.strip())
        f.write("\n```\n\n")
        f.write("---\n\n")

print(f"\n\nReport written to {OUTPUT_MD}")
sys.exit(0 if failed == 0 else 1)
