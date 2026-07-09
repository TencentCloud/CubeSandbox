# Node.js Web Sandbox

[中文文档](README_zh.md)

A minimal Node.js 20 web development sandbox for CubeSandbox. It shows how to
build a Cube-ready template from an OCI image, launch a sandbox with the
E2B-compatible SDK, run a command through envd, and reach a web service exposed
from inside the MicroVM.

Use this example when you need a small web runtime starting point rather than a
Python code-interpreter, browser automation, benchmarking, or snapshot demo.

## What This Demonstrates

- A CubeSandbox template that inherits envd from `cubesandbox-base` on port
  `49983`; the entrypoint starts envd explicitly so the same example can be
  validated on CubeSandbox runtime images that provide envd without a Docker
  entrypoint.
- A Node.js 20 HTTP service exposed on port `3000`.
- A local validation script that creates a sandbox, runs an in-sandbox smoke
  check, calls the public service URL, and cleans up the sandbox.
- The minimum documentation, resource, security, and review evidence expected
  for a reusable template/example contribution.

## Scenario Metadata

| Field | Value |
|-------|-------|
| Slug | `node-web-sandbox` |
| Category | Web development / language runtime |
| Intended users | Developers who want a small Node.js web-service template for CubeSandbox |
| Template source | `examples/node-web-sandbox/Dockerfile` |
| Required ports | `49983` for envd, `3000` for the Node.js service |
| Minimum runnable flow | Build/publish image, create template, set `CUBE_TEMPLATE_ID`, run `python validate.py` |
| Status | Ready for local review once a CubeSandbox deployment is available |

## Prerequisites

- A running CubeSandbox deployment with CubeAPI reachable from your machine.
- `cubemastercli` installed and configured for that deployment.
- Docker or another OCI image builder.
- A registry reachable by CubeMaster nodes if you are not building directly on
  the cluster.
- Python 3.8+ on your local machine.

## Template Creation

Build the image:

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-node-web:latest \
  examples/node-web-sandbox
docker push <your-registry>/cubesandbox-node-web:latest
```

`cubesandbox-base:2026.16` is currently published for `linux/amd64`; keep the
platform flag when building from ARM development machines.

Create the CubeSandbox template:

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-node-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 3000 \
  --probe-path /health
```

Wait until the template is ready:

```bash
cubemastercli tpl watch --job-id <job-id>
```

Copy the returned `template_id`; the validation script reads it from
`CUBE_TEMPLATE_ID`.

## Environment Variables

```bash
cp .env.example .env
```

Fill these values:

| Variable | Required | Description |
|----------|----------|-------------|
| `E2B_API_URL` | Yes | CubeAPI endpoint, for example `http://<cube-host>:3000` |
| `E2B_API_KEY` | Yes | CubeAPI auth key; use a placeholder only for development deployments that accept it |
| `CUBE_TEMPLATE_ID` | Yes | Template ID returned by `cubemastercli tpl create-from-image` |
| `CUBE_SSL_CERT_FILE` | No | CA bundle for deployments using a local mkcert certificate |
| `NODE_WEB_PORT` | No | Public service port; defaults to `3000` |

Do not commit `.env`; use `.env.example` for placeholders only.

## Install Dependencies

```bash
pip install -r requirements.txt
```

## Run the Minimum Example

```bash
python validate.py
```

Expected output:

```text
Template: tpl-xxxxxxxxxxxxxxxxxxxxxxxx
CubeAPI:  http://<cube-host>:3000
Port:     3000
Sandbox:  <sandbox-id>
localhost smoke ok: hello from CubeSandbox Node.js
runtime: v20.x.x
public HTTP ok: hello from CubeSandbox Node.js
node-web-sandbox validation ok
```

The script creates a sandbox from `CUBE_TEMPLATE_ID`, runs
`smoke_test.py` inside the sandbox through envd, calls
`https://<sandbox-public-host>/api/hello`, and lets the SDK clean up the
sandbox when the `with` block exits.

## Local Container Smoke Check

You can validate the image before registering it as a CubeSandbox template:

```bash
docker run --rm -d \
  --platform linux/amd64 \
  -p 3000:3000 \
  -p 49983:49983 \
  --name cube-node-web \
  <your-registry>/cubesandbox-node-web:latest

curl -s http://127.0.0.1:3000/api/hello
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

docker rm -f cube-node-web
```

Expected results:

- `/api/hello` returns JSON with `ok: true` and
  `message: "hello from CubeSandbox Node.js"`.
- envd `/health` returns HTTP `204`.

## Resource Guidance

- Writable layer: `1G` is enough for this minimal service and smoke test.
- CPU and memory: baseline sandbox resources are enough for the default flow.
- Timeout: `120` seconds is enough for validation once the template is ready.
- Larger package installs, development servers with hot reload, or build tools
  may need more writable layer space and a longer timeout.

## Security and Network Notes

- The template does not require application credentials.
- `E2B_API_KEY` is a control-plane credential and must never be committed.
- The Node.js service is exposed through CubeSandbox public URL routing on port
  `3000`; treat anything served there as externally reachable unless your
  deployment restricts public access.
- The example does not request custom outbound egress rules. It uses the
  deployment defaults.
- The Docker image keeps envd on `49983` so SDK command execution and file
  access remain available.
- Do not bake secrets into the image. Pass runtime configuration through
  sandbox environment variables when needed.

## CubeSandbox Capability Notes

- **Snapshot / resume**: This example does not demonstrate pause, resume, or
  snapshot restore. Use the snapshot and lifecycle examples for that flow.
- **Stateful workspace**: The service is stateless. Files created during a
  sandbox session are ephemeral unless you combine the template with
  CubeSandbox snapshot or storage features.
- **Multi-service coordination**: The template runs envd plus one Node.js
  service. It does not demonstrate multiple user services or sidecars.
- **Public access**: Port `3000` is intentionally exposed for web traffic. For
  private services, combine this pattern with restricted public access.
- **Restricted network operation**: The default validation flow does not set
  allowlists or denylists. Use network-policy or route-aware egress examples
  when outbound controls are the primary scenario.

Relevant guides:

- [Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
- [Template Inspection and Request Preview](../../docs/guide/template-inspection-and-preview.md)
- [Network Policy](../../docs/guide/network-policy.md)
- [Restrict Public Access](../../docs/guide/restrict-public-access.md)
- [Snapshot, Rollback, and Clone](../../docs/guide/snapshot-rollback-clone.md)

## Known Limitations

- The example assumes the image is reachable by CubeMaster nodes.
- The validation script expects public URL access to port `3000` unless your
  deployment supplies equivalent routing.
- The image installs Node.js 20 from NodeSource during image build. If your
  build environment cannot reach NodeSource, mirror the package repository or
  build from a pre-seeded internal base image.
- Local Docker validation proves the service and envd start, but the full
  acceptance flow requires a ready CubeSandbox template.

## Contributor Artifact Checklist

Use this example as the minimum pattern for future ecosystem entries:

- [x] One self-contained directory under `examples/<slug>/`.
- [x] Template source through a Dockerfile or documented image.
- [x] Build or acquisition path for the template.
- [x] Minimum runnable example flow.
- [x] `README.md` with purpose, prerequisites, commands, expected output,
  resource guidance, security notes, known limitations, and cleanup.
- [x] Dependency declarations for the local runner.
- [x] Environment example with placeholders only.
- [x] Documentation registration in the examples index.
- [x] Reviewer-verifiable validation evidence.

## Maintainer Review Checklist

Use this checklist with the planning contracts in
`specs/001-sandbox-template-ecosystem/contracts/`:

- [ ] The template source is reproducible and names all required ports.
- [ ] The README can be followed without hidden setup.
- [ ] Expected output is concrete enough to compare with a real run.
- [ ] Resource guidance covers the minimum runnable example.
- [ ] Security notes cover credentials, public access, external access, and
  secret handling.
- [ ] The catalog entry is distinct from existing examples and points to the
  correct path.
- [ ] Validation evidence is redacted and repeatable.
- [ ] The example does not weaken CubeSandbox authentication, egress, resource,
  or isolation controls.

## Duplicate Scenario Guidance

This entry is intentionally different from:

- `code-sandbox-quickstart`: Python code and shell execution basics.
- `browser-sandbox`: Chromium and Playwright/CDP automation.
- `openai-agents-code-interpreter`: data-analysis agent and Jupyter-style code
  execution.
- `cubesandbox-base-nginx`: static nginx service smoke test.

If a future contribution is another Node.js web example, it should either add a
clear differentiator such as a framework, stateful workspace, restricted
network mode, or multi-service setup, or revise this example instead of adding
a duplicate.

## Validation Evidence

Template for reviewer evidence:

```text
validated_on: 2026-07-09
validated_by: <contributor-or-maintainer>
deployment: <single-node|multi-node|other>, <host-arch>
template_build: cubemastercli tpl create-from-image ... -> template_status READY
template_id: tpl-<redacted>
environment: E2B_API_URL=<redacted>, E2B_API_KEY=<redacted>, CUBE_TEMPLATE_ID=tpl-<redacted>
install: pip install -r requirements.txt
smoke_test: python validate.py
observed_output: node-web-sandbox validation ok
cleanup: sandbox deleted by SDK context manager
limitations: <anything discovered during validation>
```

Current repository validation:

```text
validated_on: 2026-07-09
validated_by: Codex
scope: repository static checks, local Docker smoke validation, and CubeSandbox template validation
syntax_checks: node --check server.js; python3 -m py_compile smoke_test.py validate.py
image_build: docker build --build-arg CUBE_BASE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest -t cubesandbox-node-web:validation . -> success
image_digest: sha256:1016a5a28e20cbe9c56aa15d52a239bedf124ac48183c6c4c925c69abd3a2c57
container_smoke: python3 smoke_test.py -> localhost smoke ok: hello from CubeSandbox Node.js
envd_health: GET http://127.0.0.1:49983/health -> 204
registry_image: 127.0.0.1:5000/cubesandbox-node-web:validation
deployment: single-node one-click CubeSandbox test deployment, Linux x86_64
template_build: cubemastercli -a 127.0.0.1 -p 8089 --timeout 60s tpl create-from-image --image 127.0.0.1:5000/cubesandbox-node-web:validation --writable-layer-size 1G --expose-port 49983 --expose-port 3000 --probe 3000 --probe-path /health -> READY
job_id: a5708319-9944-4c18-a9da-335a0d6b415c
template_id: tpl-32bee19794f94962a686937a
artifact_id: rfs-dc5359ab6d74b5ec26cfaa0a
install: python3 -m pip install --user -r requirements.txt -> requirements already satisfied after offline wheel bootstrap
smoke_test: E2B_API_URL=http://127.0.0.1:3000 E2B_API_KEY=<redacted> CUBE_TEMPLATE_ID=tpl-32bee19794f94962a686937a python3 validate.py
observed_output: Sandbox deed131c0f5c42228bb7abb396522eab; runtime v20.20.2; public HTTP ok; node-web-sandbox validation ok
cleanup: sandbox deleted by SDK context manager
limitations: remote host DNS was temporarily unavailable during first dependency download, so e2b wheels were bootstrapped offline before rerunning the documented install command
```

## Cleanup

Sandbox cleanup is automatic when `validate.py` exits the SDK context manager.
Remove any local container used for preflight validation with:

```bash
docker rm -f cube-node-web
```

Delete test templates only when no longer needed:

```bash
cubemastercli tpl delete --template-id <template-id>
```
