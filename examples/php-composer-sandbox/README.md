# PHP + Composer Sandbox Template

[中文文档](README_zh.md)

A CubeSandbox-ready PHP web-development template with Composer. It starts a
minimal JSON API on port `8080`, while the inherited `cubesandbox-base` image
keeps envd healthy on port `49983`. The two host-side scripts demonstrate
public-port access and stateful workspace recovery after pause/resume.

## What is included

- PHP CLI, `php-mbstring`, `php-xml`, and Composer on Ubuntu 22.04.
- A deliberately small PHP router with `/health`, `/api/hello`, and a
  stateful `/api/state` endpoint that writes to `/workspace/state.json`.
- `run_example.py`: creates a sandbox, verifies the installed PHP/Composer
  versions, and calls the API through CubeProxy.
- `resume_example.py`: writes application state, pauses the MicroVM, reconnects
  it, and proves the state file survives.

## Prerequisites

- A running CubeSandbox deployment and `cubemastercli` connected to it.
- Docker plus an image registry reachable from every CubeMaster node.
- Python 3.9+ on the host for the two driver scripts.

The template ships no third-party PHP package on purpose: it is a clean
Composer starting point. Add dependencies to `app/composer.json`, commit the
resulting `composer.lock`, then rebuild the image.

## 1. Build and publish the image

From the repository root:

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/php-composer-cube:latest \
  examples/php-composer-sandbox
docker push <your-registry>/php-composer-cube:latest
```

If the build workstation cannot reliably reach Ubuntu's default archive, pass
an Ubuntu mirror only for that build, for example
`--build-arg APT_MIRROR=https://<reachable-mirror>/ubuntu`.

For a local container smoke test, expose both the app and inherited envd ports:

```bash
docker run --rm -d --name php-composer-cube \
  -p 8080:8080 -p 49983:49983 \
  php-composer-cube:latest
curl -fsS http://127.0.0.1:8080/health
curl -s -o /dev/null -w 'envd=%{http_code}\n' http://127.0.0.1:49983/health
docker stop php-composer-cube
```

Both HTTP checks must succeed; envd returns `204`.

## 2. Register the Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/php-composer-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job-id>
```

Use the printed template ID only after the job reports `template_status: READY`.
Port `49983` is used only for the platform readiness probe; port `8080` is the
application API.

## 3. Run the examples

```bash
cd examples/php-composer-sandbox
cp .env.example .env
# Set E2B_API_URL and CUBE_TEMPLATE_ID in .env.
python3 -m pip install -r requirements.txt

# Creates a disposable sandbox and calls /health and /api/hello.
python3 run_example.py

# Writes /workspace/state.json, pauses, reconnects, then verifies the state.
python3 resume_example.py
```

The scripts delete their sandbox in a `finally`/context-manager cleanup path.

## Resource and security notes

- Start with `2G` writable storage. Raise it if Composer dependencies or build
  artifacts are stored in the image/workspace.
- The sample needs no outbound network traffic at runtime. If you install
  Composer packages inside a sandbox, give that workflow an explicit egress
  allowlist rather than unrestricted internet access.
- The public API is intentionally unauthenticated and is suitable only as an
  example. For a real service, set `network={"allow_public_traffic": False}`
  when creating the sandbox and send the per-sandbox traffic token, as shown in
  `examples/code-sandbox-quickstart/restrict_public_access.py`.
- A pause keeps the VM snapshot and writable workspace; a kill permanently
  deletes both. Do not use a context manager around a sandbox you plan to
  reconnect after a pause.

## Verification

These checks do not require a Cube cluster:

```bash
python3 -m unittest discover -s examples/php-composer-sandbox -p 'test_*.py'
python3 -m py_compile examples/php-composer-sandbox/*.py
git diff --check
```
