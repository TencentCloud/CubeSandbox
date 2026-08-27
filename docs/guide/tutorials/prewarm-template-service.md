# Pre-warm a Service in a Template

A regular template can preinstall an operating system and dependencies, but an application may still perform expensive initialization after every sandbox starts—for example, loading an SDK, creating an agent session, scanning a workspace, or building a local cache. CubeSandbox can wait for that work to finish before taking the template snapshot, allowing new sandboxes to restore an already initialized process and memory state.

This guide uses [`examples/pi-agent-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration) to demonstrate the pattern. Read [Templates Overview](../templates.md) first if you are unfamiliar with how a probe determines when the snapshot is taken.

## How It Works

The key to a pre-warmed template is an accurate readiness endpoint:

1. The image starts a resident service.
2. The service performs the initialization that should be pre-warmed.
3. Its readiness endpoint returns HTTP 2xx only after initialization completes.
4. CubeSandbox observes the successful probe and saves the filesystem, memory, and process state.
5. Sandboxes created from the template restore the resident service ready to accept work.

The readiness endpoint should not succeed merely because the HTTP server has started listening. Return 503 until every state that must be preserved in the snapshot is ready.

## Pi Agent Warmup Example

The regular Pi Agent workflow starts a new process for every task. The example warmup adapter instead runs as the image's resident process and creates a Pi SDK `AgentSession` during startup. `GET /readyz` returns 200 only after that session is initialized.

### 1. Prepare the Resident Service

The implementation is in [`pi_warmup_adapter.mjs`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/pi_warmup_adapter.mjs). Its core pattern is:

```javascript
let ready = false;

const server = http.createServer((request, response) => {
  if (request.method === "GET" && request.url === "/readyz") {
    response.writeHead(ready ? 200 : 503);
    return response.end();
  }

  // Handle application requests after restore.
});

session = await createAgentSession(/* ... */);
ready = true;
server.listen(8080, "0.0.0.0");
```

The complete adapter also exposes `POST /prompt` to send work to the restored resident `AgentSession`.

### 2. Make the Service the Image Command

[`Dockerfile.warmup`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/Dockerfile.warmup) builds on the Pi Agent image, copies the adapter, and sets it as the image `CMD`:

```dockerfile
ARG PI_AGENT_IMAGE=pi-agent-cube:latest
FROM ${PI_AGENT_IMAGE}

COPY pi_warmup_adapter.mjs /tmp/pi_warmup_adapter.mjs
RUN PI_PACKAGE_DIR="$(npm root -g)/@earendil-works/pi-coding-agent" \
    && install -m 0755 /tmp/pi_warmup_adapter.mjs \
       "${PI_PACKAGE_DIR}/pi_warmup_adapter.mjs" \
    && rm /tmp/pi_warmup_adapter.mjs

ENV PI_WARMUP_HOST=0.0.0.0 \
    PI_WARMUP_PORT=8080

EXPOSE 49983 8080

CMD ["sh", "-c", "exec node \"$(npm root -g)/@earendil-works/pi-coding-agent/pi_warmup_adapter.mjs\""]
```

Port `8080` serves application readiness and task requests. Port `49983` is used by `envd` from the base image to retain SDK command, file, and terminal capabilities.

### 3. Build and Push the Image

From the repository root, build the base image followed by the warmup image:

```bash
docker build --platform linux/amd64 \
  -t localhost:5000/pi-agent-cube:latest \
  examples/pi-agent-integration

docker build --platform linux/amd64 \
  -f examples/pi-agent-integration/Dockerfile.warmup \
  --build-arg PI_AGENT_IMAGE=localhost:5000/pi-agent-cube:latest \
  -t localhost:5000/pi-agent-warmup-cube:latest \
  examples/pi-agent-integration

docker push localhost:5000/pi-agent-cube:latest
docker push localhost:5000/pi-agent-warmup-cube:latest
```

Replace the example address with an image registry reachable by the CubeSandbox cluster.

### 4. Build the Template Using the Application Probe

Expose both `envd` and the warmup adapter, but point the probe at `/readyz`, which represents completion of Pi session initialization:

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-warmup-cube:latest \
  --alias pi-warmup \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 8080 \
  --probe-path /readyz
```

Do not use `49983/health` as the warmup-complete signal in this example. It only proves that `envd` is ready, not that the Pi `AgentSession` has been created.

Run `cubemastercli tpl watch --job-id <job-id>` until the job reaches `READY`. Sandboxes created from the resulting template restore the initialized Node process and Pi session.

## Designing Your Own Pre-warmed Service

Follow these principles when applying the same pattern to another service:

- **Probe the real warmup state.** Return 2xx only after model loading, runtime initialization, or cache construction has completed.
- **Keep the process resident.** The initialized process must remain running to be restored with the memory snapshot.
- **Do not bake secrets into the template.** Do not supply API keys, tokens, or user data during the build. Provide them after restore through a request, secret vault, or another runtime mechanism.
- **Treat external connections carefully.** Database connections, long-lived sockets, and temporary credentials may be invalid after restore. Detect and recreate them instead of assuming snapshotted connections remain usable.
- **Define the concurrency model.** One adapter in the Pi example owns one session and returns HTTP 409 for concurrent work. Implement a pool or assign one task per sandbox when concurrency is required.
- **Keep readiness checks lightweight.** A probe should read local state without repeating expensive initialization or producing external side effects.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Template build keeps waiting for the probe | Confirm that the service listens on `0.0.0.0`, the port and path match, and initialization logs show success. |
| Template is READY, but the first request still initializes the app | The readiness endpoint succeeds too early; set the ready state only after full initialization. |
| SDK command or file APIs are unavailable | Confirm the image contains `envd` and exposes it with `--expose-port 49983`. |
| External requests fail after restore | Check whether connections or credentials created before the snapshot expired, and recreate them after restore. |

For the complete Pi Agent build, invocation, and network-policy example, see [`examples/pi-agent-integration/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/README.md).
