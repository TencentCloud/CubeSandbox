# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
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

host = sandbox.get_host(9000)
cdp_url = f"https://{host}/cdp/"
print(f"[*] CDP URL: {cdp_url}")

with sync_playwright() as playwright:
    browser = playwright.chromium.connect_over_cdp(cdp_url)
    context = browser.new_context(ignore_https_errors=True)
    page = context.new_page()
    page.goto("http://www.tencent.com", wait_until="domcontentloaded")
    print(page.title())

sandbox.kill()
print("[+] Done.")
