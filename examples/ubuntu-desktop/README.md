# Ubuntu desktop sandbox

[中文](README_zh.md)

An Ubuntu 22.04 desktop in the browser: default GNOME, left dock, Files and Terminal, with keyboard, mouse, screenshots, and clipboard all working. The screen is served over noVNC on port `6080`.

## Try it locally

Docker must be installed.

```bash
cd examples/ubuntu-desktop
./run-local.sh
```

Once the build finishes, open [http://127.0.0.1:6080/](http://127.0.0.1:6080/) in your browser.

> The Dockerfile defaults to the Tencent Cloud Ubuntu mirror for faster builds in China. Pass `--build-arg APT_MIRROR=` to use the upstream archive.

Stop:

```bash
docker rm -f cube-ubuntu-desktop
```

## Use in Cube

### 1. Create a template

Push the image to a registry your cluster can pull, then:

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

### 2. Start a sandbox

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template="ubuntu-desktop", timeout=-1)
print("desktop:", f"https://{sb.get_host(6080)}/")
```

Open the returned URL — that's this sandbox's desktop.

### 3. Drive the desktop from code

The desktop inside the sandbox runs on `DISPLAY=:0`; pass that variable when running commands:

```python
sb.commands.run("DISPLAY=:0 gnome-screenshot -f /tmp/desk.png")
print(sb.files.read("/tmp/desk.png"))
```

## Files

| File | Role |
|------|------|
| `Dockerfile` | Desktop image |
| `start-desktop.sh` | Starts the desktop |
| `run-local.sh` | One-shot local run |
| `docker-compose.yml` | Local compose |
