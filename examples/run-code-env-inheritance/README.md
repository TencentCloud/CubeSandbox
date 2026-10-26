# Derived `sandbox-code` image: make `run_code` inherit sandbox-level environment variables

[中文](README_zh.md)

This example provides a derived image based on the official `sandbox-code` image. It replaces only the lightweight code interpreter so `run_code` can read sandbox-level environment variables injected through `Sandbox.create(envs=...)`, while preserving temporary per-call environment overrides.

## Background

Environment variables passed to `Sandbox.create(envs=...)` are available to `commands.run`, but `run_code` in the official `sandbox-code` image cannot read them because its lightweight code interpreter does not inject the sandbox environment from envd into the Jupyter kernel.

This derived image aligns the environments inherited by `run_code` and `commands.run` without modifying the default `sandbox-code` image or any existing templates.

## Environment semantics

Before the first user execution in each Jupyter kernel, the lightweight code interpreter reads and caches the sandbox-level environment variables from `http://127.0.0.1:49983/envs`, then injects them into the kernel. Each `run_code` call applies its `env` or `env_vars` values afterward, with the following precedence:

```text
Sandbox.create(envs=...) < per-call run_code environment variables
```

Sandbox-level values and per-call overrides are applied in a separate background kernel execution before the user code, so a compilation failure in the first user cell does not prevent environment initialization. Before applying per-call values, the interpreter snapshots the previous kernel value of each affected key. Afterward, a background cleanup restores the environment:

- Keys that existed before the call are restored to their previous kernel values.
- Keys that did not exist before the call are removed.

The next execution waits for pending cleanup so per-call values cannot leak into later executions. This lifecycle follows E2B code-interpreter. Sandbox-level environment variables are loaded only once per Jupyter kernel; this example does not provide a general runtime refresh mechanism.

This derived interpreter requires envd's `/envs` endpoint. If envd is unavailable, the endpoint is unsupported, the request exceeds `ENVD_TIMEOUT` (2 seconds by default), or the payload is invalid, `run_code` fails with HTTP 502 instead of falling back to the stock interpreter behavior. Set `ENVD_TIMEOUT` in the image environment to adjust the fetch timeout.

## Build and test the image

From the repository root:

```bash
docker build \
  -t sandbox-code-env-inheritance:latest \
  examples/run-code-env-inheritance
```

Override `SANDBOX_CODE_IMAGE` when using the international registry or a pinned base image:

```bash
docker build \
  --build-arg SANDBOX_CODE_IMAGE=cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  -t sandbox-code-env-inheritance:latest \
  examples/run-code-env-inheritance
```

After building the image, run the focused unit tests inside it so they use the same Python dependencies as the runtime:

```bash
docker run --rm \
  --entrypoint python \
  -v "$PWD/examples/run-code-env-inheritance:/work:ro" \
  -e PYTHONPATH=/work/lightweight-code-interpreter \
  sandbox-code-env-inheritance:latest \
  -m unittest discover -s /work/tests -v
```

## Create a CubeSandbox template

Push the image to a registry reachable by CubeMaster, then create the template:

```bash
cubemastercli tpl create-from-image \
  --image <registry>/sandbox-code-env-inheritance:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49999 \
  --probe-path /health
```

Watch the asynchronous build with the `job_id` returned by the create command:

```bash
cubemastercli tpl watch --job-id <job_id>
```

### Use a local image in one-click deployments

If CubeMaster cannot access an image registry, build or load `sandbox-code-env-inheritance:latest` into Docker on the one-click control-plane host. Add the following setting to `/usr/local/services/cubetoolbox/.one-click.env`:

```bash
CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED=false
```

Restart CubeMaster to apply the setting:

```bash
systemctl restart cube-sandbox-cubemaster.service
```

This disables the default native rootfs exporter and falls back to the Docker-based exporter, which can consume the local Docker image. Create the template with the local image tag:

```bash
cubemastercli tpl create-from-image \
  --image sandbox-code-env-inheritance:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49999 \
  --probe-path /health
```

## Run the SDK compatibility E2E test

After creating the template, run the shared CubeSandbox/E2B compatibility case to verify sandbox-level inheritance and per-call merge, override, and cleanup behavior:

```bash
export CUBE_TEMPLATE_ID=<template_id>
cd tests/e2e/sdk_compat
SDK_E2E_RUN_CODE_ENV_INHERITANCE=true \
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e \
  cases/run_code/test_python.py::test_run_code_merges_create_and_per_call_envs -q
```

`CUBE_TEMPLATE_ID` reuses the SDK compatibility suite's existing template selection mechanism; `--cube-template-id <template_id>` is also supported. The environment-inheritance case is skipped by default before creating a sandbox and runs only when `SDK_E2E_RUN_CODE_ENV_INHERITANCE=true` is explicitly set for a compatible template.
