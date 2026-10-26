# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b import Sandbox
from playwright.sync_api import sync_playwright
from dotenv import load_dotenv
from pathlib import Path

load_dotenv(dotenv_path=Path(__file__).with_name(".env"),override=False)
# os.environ["E2B_API_KEY"] = "e2b_000000"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["NODE_EXTRA_CA_CERTS"] = "/root/.local/share/mkcert/rootCA.pem"
os.environ["NODE_NO_WARNINGS"] = "1"

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    print(sandbox.get_info())

    cdp_url = f"https://{sandbox.get_host(9000)}/cdp?"
    # use playwright to operate the browser through cdp_url
    with sync_playwright() as playwright:
        browser = playwright.chromium.connect_over_cdp(
            cdp_url,
        )
        context = browser.new_context(ignore_https_errors=True)
        page = context.new_page()
        page.goto("http://www.tencent.com", wait_until="domcontentloaded")
        print(page.title())
