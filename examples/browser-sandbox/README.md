# Browser Sandbox (Playwright)

[中文文档](README_zh.md)

Run a headless Chromium browser inside a Cube Sandbox and control it remotely
with [Playwright](https://playwright.dev/) via the Chrome DevTools Protocol (CDP).

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform fully compatible with the
[E2B SDK](https://e2b.dev). The browser sandbox image ships with Chromium
started in **remote-debugging mode** on port `9000`. CubeProxy routes the CDP
WebSocket endpoint through the standard `<port>-<sandbox_id>.<domain>` URL
scheme, so Playwright can attach from any machine without custom networking.

```
Your script
    │  Playwright CDP (WebSocket)
    ▼
CubeProxy ── https://<sandbox_id>-9000.<domain>/cdp?
    │
    ▼
Sandbox VM (Chromium, port 9000)
```

## 2. Use Cases

- Web scraping in an isolated, disposable environment
- Automated UI testing against arbitrary websites
- Screenshot / PDF generation services
- LLM agent browsing tasks (each agent run gets a fresh browser VM)

## 3. Architecture

```
┌──────────────────────┐         ┌─────── Cube Sandbox ──────────────┐
│                      │         │                                    │
│  Your Script         │  CDP WS │  ┌───────────────────────────┐    │
│  (Playwright Python) │────────►│  │  Chromium --remote-debug  │    │
│                      │  HTTPS  │  │  port 9000                │    │
│                      │         │  └───────────────────────────┘    │
└──────────────────────┘         │                                    │
                                 │  CubeProxy (TLS termination)       │
                                 └────────────────────────────────────┘
```

| Component | Description |
|-----------|-------------|
| **Cube Sandbox** | KVM MicroVM booted from the browser template |
| **Chromium** | Pre-installed, launched at boot with `--remote-debugging-port=9000` |
| **CubeProxy** | Routes `<port>-<sandbox_id>.<domain>` to the correct VM port |
| **Playwright** | Attaches via CDP — no browser installation required on the host |

## 4. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+

```bash
pip install -r requirements.txt
playwright install chromium
```

## 5. Quick Start

### Step 1 — Create the Browser Template

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /cdp/json/version
```

> **Image registry:** Use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` (recommended for international access). If you are in mainland China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` instead.

Note the `template_id` printed on success.

### Step 2 — Configure Environment Variables

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID
```

Or export directly:

```bash
export E2B_API_KEY=dummy
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>

# Only needed when using Cube's built-in mkcert certificate:
# export NODE_EXTRA_CA_CERTS=/root/.local/share/mkcert/rootCA.pem
```

### Step 3 — Run the Example

```bash
python browser.py
```

Expected output:

```
SandboxInfo(sandbox_id='...', template_id='...', ...)
腾讯网
```

## 6. How the Script Works

```python
sandbox = Sandbox.create(template=template_id)
cdp_url = f"https://{sandbox.get_host(9000)}/cdp?"

with sync_playwright() as playwright:
    browser = playwright.chromium.connect_over_cdp(cdp_url)
    ...
```

| Step | Code | Description |
|------|------|-------------|
| 1 | `Sandbox.create(template=...)` | Boots a new MicroVM from the browser template |
| 2 | `sandbox.get_host(9000)` | Resolves the CubeProxy URL for port 9000 of this sandbox |
| 3 | `connect_over_cdp(cdp_url)` | Playwright attaches to the already-running Chromium process |
| 4 | `page.goto(...)` | Full Playwright API — navigate, click, screenshot, scrape, etc. |

## 7. Going Further

```python
# Take a screenshot
page.screenshot(path="screenshot.png")

# Run JavaScript
title = page.evaluate("document.title")

# Wait for an element
page.wait_for_selector("#main-content")

# Fill a form and submit
page.fill('input[name="q"]', "cube sandbox")
page.press('input[name="q"]', "Enter")
page.wait_for_load_state("networkidle")
```

Refer to the [Playwright Python docs](https://playwright.dev/python/docs/api/class-page) for the full API.

## 8. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `Error: connect ECONNREFUSED` | CubeAPI not reachable | Check `E2B_API_URL` and that port 3000 is open |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA cert | Set `NODE_EXTRA_CA_CERTS=/root/.local/share/mkcert/rootCA.pem` |
| `Timeout waiting for CDP` | Chromium not yet ready | The browser image starts Chromium at boot; retry or increase timeout |
| `Template not found` | Wrong template ID | Re-run `cubemastercli tpl list` to verify the ID |

## 9. Directory Structure

```
browser-sandbox/
├── README.md           # English documentation (this file)
├── README_zh.md        # Chinese documentation
├── browser.py          # Main example script
├── requirements.txt    # Python dependencies
└── .env.example        # Environment variable template
```
