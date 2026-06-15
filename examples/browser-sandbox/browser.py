# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import json
import os
import time
import urllib.parse
import urllib.request
from cubesandbox import Sandbox
from playwright.sync_api import sync_playwright
from dotenv import load_dotenv
from pathlib import Path

load_dotenv(dotenv_path=Path(__file__).with_name(".env"), override=False)
os.environ["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"
os.environ["NODE_NO_WARNINGS"] = "1"

template_id = os.environ["CUBE_TEMPLATE_ID"]

sandbox = Sandbox.create(template=template_id)
print(sandbox.get_info())

# ── VNC setup ─────────────────────────────────────────────────
# The browser template image already ships with Xvfb (display :1),
# x11vnc (port 5901) and noVNC/websockify (port 6901) pre-installed
# and running.  We only need to:
#   1. Replace the headless Chromium with a visible one on :1
#   2. Start websockify on the exposed port 6080 → VNC 5901
print("[*] Setting up visible Chromium + VNC...")

# Kill the existing headless Chromium so we can restart it with a GUI.
sandbox.commands.run("pkill -f 'chromium.*remote-debugging' || true", timeout=5)
time.sleep(1)

# Restart Chromium on the template's display :1 with the demo page.
# Use CDP port 9222 (the image's CDP_PORT env); nginx on 9000 proxies
# /cdp/* → 127.0.0.1:9222.
DEMO_URL = "http://www.tencent.com"
sandbox.commands.run(
    "bash -c 'DISPLAY=:1 nohup chromium --no-sandbox "
    "--remote-debugging-port=9222 --remote-debugging-address=0.0.0.0 "
    "--disable-gpu --no-first-run --disable-dev-shm-usage "
    f"{DEMO_URL} "
    "> /dev/null 2>&1 &'",
    timeout=10,
)
time.sleep(3)

# Start websockify on the exposed port 6080 → existing x11vnc on 5901.
# (The image's noVNC on 6901 is not exposed, so we bridge 6080 → 5901.)
sandbox.commands.run(
    "bash -c 'nohup websockify --web=/usr/share/novnc "
    "6080 localhost:5901 > /dev/null 2>&1 &'",
    timeout=10,
)
time.sleep(1)

proxy_ip = os.environ.get("CUBE_PROXY_NODE_IP", "127.0.0.1")
proxy_port = os.environ.get("CUBE_PROXY_PORT_HTTP", "80")
sid = sandbox.sandbox_id

novnc_url = f"http://{proxy_ip}:{proxy_port}/sandbox/{sid}/6080/vnc.html?autoconnect=true"
print(f"[*] noVNC URL: {novnc_url}")

# ── Playwright CDP ──────────────────────────────────────────
# CubeProxy path-based routing does NOT rewrite webSocketDebuggerUrl inside
# the CDP JSON response body (proxy_redirect only touches Location headers).
# So we fetch the CDP info ourselves, rewrite the WS URL to include the
# /sandbox/<id>/<port>/ prefix, and pass the corrected ws:// URL directly.
cdp_version_url = f"http://{proxy_ip}:{proxy_port}/sandbox/{sid}/9000/cdp/json/version"
print(f"[*] Fetching CDP version info from {cdp_version_url}")
resp = urllib.request.urlopen(cdp_version_url, timeout=10)
cdp_info = json.loads(resp.read().decode())
raw_ws_url = cdp_info.get("webSocketDebuggerUrl", "")
print(f"[*] Raw WS URL from Chromium: {raw_ws_url}")

if not raw_ws_url:
    raise RuntimeError("Chromium did not return a webSocketDebuggerUrl")

# Rewrite: ws://<any_host>/cdp/devtools/browser/<guid>
#      --> ws://<proxy_ip>:<proxy_port>/sandbox/<sid>/9000/cdp/devtools/browser/<guid>
parsed = urllib.parse.urlparse(raw_ws_url)
ws_url = f"ws://{proxy_ip}:{proxy_port}/sandbox/{sid}/9000{parsed.path}"
print(f"[*] Rewritten WS URL: {ws_url}")

with sync_playwright() as playwright:
    browser = playwright.chromium.connect_over_cdp(ws_url)
    context = browser.contexts[0]
    page = context.pages[0] if context.pages else context.new_page()
    # Navigate explicitly so we get the correct title and the VNC
    # display stays in sync with the Playwright page.
    page.goto(DEMO_URL, wait_until="domcontentloaded", timeout=15_000)
    print(f"[*] Page title: {page.title()}")
    page.screenshot(path="screenshot.png")
    print("[*] Screenshot saved to screenshot.png")

# The sandbox is intentionally left running so the noVNC iframe in the
# web UI can connect to the live Chromium desktop.  The platform will
# reclaim the sandbox after its TTL expires.
print("[*] Sandbox is alive — the noVNC preview below is live.")
print("[*] You can interact with the Chromium desktop in real time.")
print("[+] Done.")
