# Connect to an Existing Cube Cluster

To get started quickly, check out the example directory:

- Example: [examples/e2b-dev-sidecar](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar)
- Chinese README: [README_zh.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README_zh.md)
- English README: [README.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README.md)

## Why we need `dev-sidecar`

E2B expects sandbox URLs to resolve to the target cluster's public IP through wildcard DNS. In a production deployment, that usually means adding a private DNS A record like:

```text
*.cube.app => <your cube master node ip>
```

That is inconvenient during local development. Setting up wildcard DNS on a developer machine is usually the annoying part, so `dev-sidecar` exists to let you connect your local machine to a Cube cluster and create sandboxes without changing the E2B SDK itself.

This page only does one thing: help you quickly decide how to fill the `dev-sidecar` environment variables.

## Start With the Happy Path

### Case 1: You started Cube locally with `dev-env`

This is the most natural and recommended development path for `dev-sidecar`.

If you followed [Development Environment (QEMU VM)](./dev-environment), the defaults in `examples/e2b-dev-sidecar/env.example` were already chosen for this exact case.

Do this:

```bash
cd examples/e2b-dev-sidecar
pip install -r requirements.txt
cp env.example .env
```

Then usually you only need to fill in the template ID:

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
E2B_API_KEY="dummy"
CUBE_TEMPLATE_ID="<your-template-id>"
```

Run:

```bash
python demo.py
```

The key point is:

- `127.0.0.1:13000` is not arbitrary. It is the CubeAPI endpoint exposed by `dev-env`.
- `127.0.0.1:11443` is not arbitrary. It is the CubeProxy endpoint exposed by `dev-env`.

So if you already booted local `dev-env`, you usually do not need to change the addresses. You mostly just fill in the template ID.

### Case 2: You want to connect to a Cube cluster on another machine

You still use the same `dev-sidecar` example. You only replace the default addresses with the real endpoints of that remote cluster:

```bash
E2B_API_URL="http://<node-ip>:3000"
CUBE_REMOTE_PROXY_BASE="https://<node-ip>:443"
E2B_API_KEY="dummy"
CUBE_TEMPLATE_ID="<your-template-id>"
```

Then run the same command:

```bash
python demo.py
```

## Just Remember These

- `E2B_API_URL`
  Control-plane endpoint. In `dev-env`, the default is `http://127.0.0.1:13000`.
- `CUBE_REMOTE_PROXY_BASE`
  Data-plane endpoint. In `dev-env`, the default is `https://127.0.0.1:11443`.
- `E2B_API_KEY`
  Must be non-empty for the SDK. If auth is enabled, use the real key.
- `CUBE_TEMPLATE_ID`
  The template ID used when creating the sandbox.

You usually do not need to think about the other variables first. For most development flows, getting these four values right is enough.

## Common Mistakes

- You are using local `dev-env`, but configured the in-VM addresses instead of the host-exposed `13000/11443` ports
- You pointed `CUBE_REMOTE_PROXY_BASE` at the sidecar's own listening address instead of CubeProxy
- You forgot to set `CUBE_TEMPLATE_ID`
- Auth is enabled on the cluster, but `E2B_API_KEY` is still `dummy`

## Further Reading

If you want the easiest explanation, go straight to the example README:

- [examples/e2b-dev-sidecar/README.md](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar/README.md)

If you are ready to wire the sidecar into your own code, then look at:

- `demo.py`
- `dev_sidecar.py`
