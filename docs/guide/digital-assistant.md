# Digital Assistant

The Digital Assistant (AgentHub) uses Cube Sandbox to create and manage OpenClaw assistants. It supports assistant instances, snapshots, rollback, clone creation, assistant template publishing, and operation history.

::: warning Preview
The Digital Assistant is a preview feature intended for demos and early validation. APIs, database schema, deployment options, and UX details may still change in later releases. Validate it in a non-production environment before production use.
:::

## Digital Assistant Template

AgentHub creates assistants from a CubeSandbox template. Before deployment, build the Digital Assistant template (see the command below), then copy the auto-generated `tpl-` prefixed template ID into `.env`:

```env
AGENTHUB_DS_OPENCLAW_TEMPLATE=<your-digital-assistant-template-id>
```

A custom template must be built from the **same Digital Assistant / OpenClaw image** as `wecom-ds-openclaw`. The image is expected to contain the OpenClaw runtime, `supervisorctl` service wiring, and the ports used by AgentHub:

- OpenClaw Gateway UI: `18789`
- assistant environment UI: `8080`

The default template is built from an all-in-one OpenClaw image, which is relatively large. Initial template creation or rebuilds need enough space for image download, extraction, snapshotting, and distribution. In typical demo environments, creating the template takes about 15 minutes; actual time depends on image cache state, disk performance, and node count. Before building the template, make sure the host and Cubelet data disk have enough free space to avoid failures caused by running out of disk.

Use the following rough estimate for disk space planning:

- One template is about `3 GB` (rootfs `1G` + memory `2G`).
- One snapshot is about `2~3 GB` (memory is always `2G` plus the rootfs delta).
- One running instance mainly uses reflink deltas, usually only tens of MB.
- Docker infrastructure is about `3.2 GB` as fixed overhead.

If you keep only `1` template, `2` snapshots, and a few running instances, reserve about `12~15 GB` of free disk space.

Build or re-create the template with `cubemastercli tpl create-from-image` using the same image:

```bash
OPENCLAW_IMAGE=cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest

cubemastercli tpl create-from-image \
  --image "${OPENCLAW_IMAGE}" \
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

In one-click deployments, when `DATABASE_URL` is omitted, the startup script builds it from `CUBE_SANDBOX_MYSQL_*`.

### LLM API Key

Before creating a digital assistant, configure the LLM API key (and provider, base URL, model) on the **AgentHub settings** page in the WebUI.

You cannot create or reconfigure an assistant until this is done; the UI will prompt you to finish setup first.

Once configured, CubeAPI injects the key into OpenClaw inside the sandbox and writes the relevant config files (such as `auth-profiles.json`) so the assistant can reach the LLM service.

## Template Fast Path

When creating a new assistant from a published assistant template, and no WeCom re-binding is required, CubeAPI uses a template fast path. The new sandbox reuses the OpenClaw configuration already stored in the template snapshot, so CubeAPI does not inject the LLM API key again.

## Security Notes

- Keep your LLM API key confidential; do not commit it to Git.
- Protect database backups and access (the key is stored in the database).
