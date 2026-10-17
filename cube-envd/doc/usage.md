# Create a template and use cube-envd

[中文](usage_zh.md) · [Component overview](../README.md)

Run commands from the repository root. First build an image using the [README](../README.md#build-a-sandbox-image).

Configure `cubemastercli` for the existing deployment, then create a template
from an image accessible to its builder. The local-tag example below requires a
Docker exporter using the same Docker image store as the image build. The default
native exporter pulls from a registry and does not read local Docker tags.
Local Docker export requires `CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED=false` in
the build service and no higher-priority skopeo/umoci tools; see
[selected envd acceptance](../../tests/e2e/sdk_compat/README.md#selected-envd-acceptance).
With the default exporter, distribute the image through your existing registry
and replace the `--image` value below with that accessible reference.

```bash
cubemastercli tpl create-from-image \
  --image cubesandbox-demo-nginx:rust-local \
  --cpu 1000 --memory 512 \
  --writable-layer-size 1G \
  --expose-port 49983 --expose-port 80 \
  --probe 49983 --probe-path /health
```

Otherwise use an image reference available through your deployment's existing
image distribution mechanism. See the
[custom image tutorial](../../docs/guide/tutorials/bring-your-own-image.md) for CLI
configuration and image access. The CLI watches the build by default; wait for
successful completion and a **READY** template. The readiness probe is envd's
`49983/health`, not nginx's port 80 or a Jupyter endpoint. Keep the resulting
template ID for SDK use.

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e ./sdk/python

# Replace these values for your deployment and the template just created.
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-replace-with-your-template
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_PROXY_PORT_HTTP=80
export CUBE_SANDBOX_DOMAIN=cube.app
export NO_PROXY=localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
```

`CUBE_PROXY_NODE_IP` is a CubeProxy node, not the guest IP. Omit it when the
deployment's sandbox DNS already routes correctly. Provide `CUBE_API_KEY` when
the platform requires authentication; for a private HTTPS CA, configure
`SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` for the SDK's HTTP clients.

```python
import os
from cubesandbox import Sandbox, Template

template_id = os.environ["CUBE_TEMPLATE_ID"]
assert Template.get(template_id).status == "READY"
sandbox = Sandbox.create(timeout=300)
try:
    result = sandbox.commands.run("printf 'hello from cube-envd'", timeout=30)
    assert result.exit_code == 0
    assert result.stdout == "hello from cube-envd"
    assert result.stderr == ""

    content = "hello from the SDK\n你好\n"
    sandbox.files.write("/tmp/envd-example.txt", content)
    assert sandbox.files.read("/tmp/envd-example.txt") == content
    print(sandbox.sandbox_id, result.stdout)
finally:
    sandbox.kill()
```

The example removes its sandbox and leaves the template available for reuse.
After all its sandboxes are gone, remove the template when no longer needed with
`Template.delete(template_id)`. To run the complete image-to-template-to-sandbox
flow with health 204, running-daemon identity, command/file assertions, logs and
automatic cleanup, use the existing
[selected-envd acceptance entry point](../../tests/e2e/sdk_compat/README.md#selected-envd-acceptance).
It also provides three independently runnable scenarios.
