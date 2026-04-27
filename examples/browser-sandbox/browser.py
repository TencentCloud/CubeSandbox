# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b import Sandbox
from playwright.sync_api import sync_playwright

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["NODE_EXTRA_CA_CERTS"] = "/root/.local/share/mkcert/rootCA.pem"
os.environ["NODE_NO_WARNINGS"] = "1"

template_id = os.environ["CUBE_TEMPLATE_ID"]

sandbox = Sandbox.create(template=template_id)
print(sandbox.get_info())

cdp_url = f"https://{sandbox.get_host(9000)}/cdp?"
# 使用playwright通过cdp_url来操作浏览器
from playwright.sync_api import sync_playwright
with sync_playwright() as playwright:
    browser = playwright.chromium.connect_over_cdp(
        cdp_url,
    )
    context = browser.new_context(ignore_https_errors=True)
    page = context.new_page()
    page.goto("http://www.tencent.com", wait_until="domcontentloaded")
    print(page.title())
