# Digital Assistant

The Digital Assistant (AgentHub) uses Cube Sandbox to create and manage OpenClaw assistants. It supports assistant instances, snapshots, rollback, clone creation, assistant template publishing, and operation history.

::: warning Preview
The Digital Assistant is a preview feature intended for demos and early validation. APIs, database schema, deployment options, and UX details may still change in later releases. Validate it in a non-production environment before production use.
:::

## Digital Assistant Template

AgentHub creates assistants from a CubeSandbox template. Before deployment, build the Digital Assistant template (see the command below), then register it with AgentHub. Building it is not enough on its own: AgentHub only picks templates that are registered with it. See [Choosing a Template When None Is Specified](#choosing-a-template-when-none-is-specified).

::: tip
Earlier versions of this guide asked you to put the template ID in `.env` as `AGENTHUB_DS_OPENCLAW_TEMPLATE`. Nothing reads that variable, so setting it has no effect. Register the template instead.
:::

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

## Choosing a Template When None Is Specified

When a create request names neither `templateId` nor a snapshot, AgentHub picks the template in this order:

1. The template marked **Recommended** on the AgentHub page (the **Recommend** action on a template). If several are marked, the most recently registered of them.
2. Otherwise, the most recently registered template.
3. Otherwise, the built-in identifier `wecom-ds-openclaw`, which CubeMaster resolves as a template alias. It only resolves on an install where a template carries that alias; a standard install does not create one.

Registering a template therefore changes the default: once any template is registered, it is used instead of the `wecom-ds-openclaw` alias.

### "no agent template is registered"

If nothing is registered and the built-in identifier does not resolve either, creation fails with HTTP `400`:

```text
no agent template is registered: register one from the template market (POST /api/v1/agenthub/templates/market) or publish one from a running agent (POST /api/v1/agenthub/instances/{agentID}/publish-template), or pass templateId explicitly
```

Any one of the following fixes it:

- **Template Store (WebUI).** Click **Enable Assistant** on an installed OpenClaw template, or **Install and Enable Assistant** to build and register it in one step. The install dialog registers the template only after the build finishes: if you close it while the image is still downloading, the template ends up ready in CubeSandbox but not registered with AgentHub. Open the Template Store again and click **Enable Assistant** on it.
- **API.** Register an existing template with `POST /api/v1/agenthub/templates/market`. Only `templateId` is required, for example `{"templateId": "<tpl-id>", "name": "OpenClaw"}`. The endpoint does not check that the template exists in CubeSandbox, so a mistyped ID is accepted and, as the newest registration, becomes the default.

  A template ID whose registration was removed earlier cannot currently be registered again, whether through this endpoint or **Enable Assistant**. The removed registration still holds the ID, so the request fails with HTTP `500` and a duplicate-key error. Register a different template, or publish one from an assistant.
- **Publish from an assistant.** On an existing assistant, **Publish assistant template** publishes one of its snapshots as a template (`POST /api/v1/agenthub/instances/{agentID}/publish-template`).
- **Per request.** Pass `templateId` explicitly; the registry is then not consulted.

Registering from the Template Store does not mark the template **Recommended**. Use the **Recommend** action if a specific template should stay the default after newer ones are registered.

### "the agent template selected by default could not be resolved"

If a template is registered but CubeMaster can no longer resolve it — it was deleted from CubeSandbox, the registration names a template ID that does not exist, or the snapshot behind a published template is gone — creation fails with HTTP `409`. Retrying does not help: the same template is selected again until the registry changes. It is the default described above — the template marked **Recommended**, or the most recently registered one if none is — so the AgentHub page and `GET /api/v1/agenthub/templates` show which one it is. Mark a working template **Recommended**, or pass `templateId`. You can also remove the broken registration (`DELETE /api/v1/agenthub/templates/{templateID}`), but its template ID then cannot be registered again (see above). CubeOps logs the identifier it sent together with CubeMaster's original error, which does not always name it.

Only a not-found is rewritten this way. Other failures to use the selected template — for example one that exists but has no ready replica on a healthy node — surface as HTTP `502` with CubeMaster's message, which names the template AgentHub picked. Check that template in CubeSandbox, or pass `templateId`.

## Environment Variables

### AgentHub Database

CubeOps uses MySQL to persist Digital Assistant metadata, including assistant instances, snapshots, templates, and operation history:

```bash
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

In one-click deployments, when `DATABASE_URL` is omitted, the startup script exports the `CUBE_SANDBOX_MYSQL_*` fields and CubeOps maps them directly onto its database config.

### LLM API Key

Before creating a digital assistant, configure the LLM API key (and provider, base URL, model) on the **AgentHub settings** page in the WebUI.

You cannot create or reconfigure an assistant until this is done; the UI will prompt you to finish setup first.

Once configured, CubeAPI injects the key into OpenClaw inside the sandbox and writes the relevant config files (such as `auth-profiles.json`) so the assistant can reach the LLM service.

### Credential delivery and model namespace

There are two credential delivery modes:

- **Credential hosting (recommended)**: only the **API Key** is hosted. CubeEgress injects the `Authorization` header for the configured LLM Base URL on outbound requests, so the real key never enters the sandbox and OpenClaw stores only a placeholder key.
- **Environment injection (legacy)**: writes the real API key directly into OpenClaw's environment and config. Use this only when CubeEgress is unavailable.

In both modes the model ID is normalized into a `{Provider}/{ModelID}` namespace when injected into OpenClaw: `{Provider}` comes from the AgentHub Provider setting, and the part after the slash is sent upstream as the real model name. When using a **custom upstream**, make sure the Provider and model ID match the upstream, or OpenClaw may report `Unknown model`. For example, with Provider `openai-compatible` and model `deepseek-v4-flash`, OpenClaw resolves it as `openai-compatible/deepseek-v4-flash` while the upstream receives the model name `deepseek-v4-flash`.

## Template Fast Path

When creating a new assistant from a published assistant template, and no WeCom re-binding is required, CubeAPI uses a template fast path. The new sandbox reuses the OpenClaw configuration already stored in the template snapshot, so CubeAPI does not inject the LLM API key again.

## Security Notes

- Keep your LLM API key confidential; do not commit it to Git.
- Protect database backups and access (the key is stored in the database).
