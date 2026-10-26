---
title: Ubuntu Desktop Sandbox Guide
author: fslongjin
date: 2026-09-15
tags:
  - integration
  - ubuntu-desktop
  - novnc
  - gui-automation
lang: en-US
---

# Ubuntu Desktop Sandbox

Run a full Ubuntu 22.04 GNOME desktop inside a Cube sandbox and open it in your browser. Useful for GUI automation, screenshot-based testing, agent-driven desktop tasks, or any workflow that needs a real display.

## What you get

- Ubuntu 22.04 with the default Yaru theme and left dock (Files, Terminal)
- A 1280×720 display served over noVNC on port `6080`
- Keyboard, mouse, clipboard, and screenshots all working
- Scriptable from the SDK: run commands with `DISPLAY=:0` to drive the GUI

## Prerequisites

- A Cube cluster with `cubemastercli` access
- Docker, for local testing
- The `cubesandbox` Python SDK, for programmatic access

## Quick start

### 1. Build and push the image

```bash
cd examples/ubuntu-desktop
docker build -t <registry>/ubuntu-desktop:v1 .
docker push <registry>/ubuntu-desktop:v1
```

### 2. Create the template

```bash
cubemastercli tpl create-from-image \
  --image <registry>/ubuntu-desktop:v1 \
  --alias ubuntu-desktop \
  --writable-layer-size 16Gi \
  --cpu 4000 \
  --memory 8192 \
  --expose-port 6080 \
  --expose-port 49983 \
  --probe 6080 \
  --probe-path / \
  --allow-internet-access
```

The probe targets port `6080` (noVNC), so the sandbox isn't marked ready until the desktop is actually up.

### 3. Open the desktop

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template="ubuntu-desktop", timeout=-1)
print("desktop:", f"https://{sb.get_host(6080)}/")
```

Open the printed URL in your browser — you'll see the Ubuntu desktop.

## Drive the desktop from code

The desktop runs on `DISPLAY=:0` inside the sandbox. Pass that variable when running commands:

```python
# Take a screenshot
sb.commands.run("DISPLAY=:0 gnome-screenshot -f /tmp/desk.png")
print(sb.files.read("/tmp/desk.png"))

# List open windows
sb.commands.run("DISPLAY=:0 wmctrl -l")
```

## Access modes

CubeProxy serves the desktop in two ways:

- **Host mode** (default): `https://6080-<sandbox-id>.cube.app/` — works with noVNC's default WebSocket path. Recommended.
- **Path mode**: `https://<proxy>/sandbox/<sandbox-id>/6080/` — no wildcard DNS needed, but noVNC's WebSocket needs the full path. Pass `path=sandbox/<id>/6080/websockify` as a query parameter, or use a small reverse proxy that sets the `Host` header.

## Local testing

Before pushing, verify the image on your own machine:

```bash
cd examples/ubuntu-desktop
./run-local.sh
# open http://127.0.0.1:6080/
```

## References

- Example repository: [examples/ubuntu-desktop](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/ubuntu-desktop)
- noVNC: [https://novnc.com](https://novnc.com)
