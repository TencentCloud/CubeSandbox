# Digital Assistant

The Digital Assistant (AgentHub) uses Cube Sandbox to create and manage OpenClaw assistants. It supports assistant instances, snapshots, rollback, clone creation, assistant template publishing, and operation history.

## Digital Assistant Template

AgentHub creates assistants from a CubeSandbox template. By default, CubeAPI uses the prebuilt Digital Assistant template:

```env
AGENTHUB_DS_OPENCLAW_TEMPLATE=wecom-ds-openclaw
```

You can override it in `.env` when a deployment uses a different published template ID:

```env
AGENTHUB_DS_OPENCLAW_TEMPLATE=<your-digital-assistant-template-id>
```

A custom template must be built from the **same Digital Assistant / OpenClaw image** as `wecom-ds-openclaw`. The image is expected to contain the OpenClaw runtime, `supervisorctl` service wiring, and the ports used by AgentHub:

- OpenClaw Gateway UI: `18789`
- assistant environment UI: `8080`

Build or re-create the template with `cubemastercli tpl create-from-image` using the same image:

```bash
OPENCLAW_IMAGE=cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest

cubemastercli tpl create-from-image \
  --image "${OPENCLAW_IMAGE}" \
  --template-id wecom-ds-openclaw \
  --writable-layer-size 20Gi \
  --expose-port 18789 \
  --expose-port 8080 \
  --probe 18789 \
  --probe-path /
```

If you intentionally use the DeepSeek-preconfigured variant, use `cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw-deepseek:latest` after confirming the tag points to the expected digest in your environment.

The command prints a build job and `template_id`; wait until the template build finishes before using AgentHub. If your cluster requires per-node template distribution, pass `--node <node-id-or-ip>` repeatedly or run the existing template redo workflow after the initial build.

After creating a sandbox from the template, validate the image layout inside the sandbox:

```bash
supervisorctl status openclaw
curl -fsS http://127.0.0.1:18789/ >/dev/null
```

If the template is missing, built from a different image, or does not include the OpenClaw service layout, assistant creation may fail during setup, restart, token reading, or gateway URL generation.

## Environment Variables

### AgentHub Database

CubeAPI uses MySQL to persist Digital Assistant metadata, including assistant instances, snapshots, templates, and operation history:

```bash
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

If `DATABASE_URL` is not set, CubeAPI also checks:

```bash
CUBE_API_DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

In one-click deployments, when `DATABASE_URL` is omitted, the startup script builds it from `CUBE_SANDBOX_MYSQL_HOST`, `CUBE_SANDBOX_MYSQL_PORT`, `CUBE_SANDBOX_MYSQL_USER`, `CUBE_SANDBOX_MYSQL_PASSWORD`, and `CUBE_SANDBOX_MYSQL_DB`.

### DeepSeek API Key

Creating or reconfiguring OpenClaw-based digital assistants requires a DeepSeek API key. CubeAPI reads the variables in this order:

```bash
AGENTHUB_DEEPSEEK_API_KEY=sk-...
# fallback:
OPENCLAW_DEEPSEEK_API_KEY=sk-...
```

CubeAPI injects the resolved key into the sandbox through envd as:

```bash
OPENCLAW_DEEPSEEK_API_KEY
```

The OpenClaw setup script inside the sandbox writes the key to:

```text
/root/.openclaw/agents/main/agent/auth-profiles.json
```

It also updates:

```text
/root/.openclaw/openclaw.json
/root/.openclaw/agents/main/agent/models.json
```

to configure the DeepSeek provider and default model.

## Template Fast Path

When creating a new assistant from a published assistant template, and no WeCom re-binding is required, CubeAPI uses a template fast path. The new sandbox reuses the OpenClaw configuration already stored in the template snapshot, so CubeAPI does not inject the DeepSeek API key again.

## Security Notes

- Do not commit real API keys to Git.
- For one-click deployments, put the key in the target machine `.env`.
- For other deployment systems, inject it through a Secret or controlled environment variable.
