# Agent tool allowlist (BYOI)

Small [Bring Your Own Image](../../docs/guide/tutorials/bring-your-own-image.md)
template for [#645](https://github.com/TencentCloud/CubeSandbox/issues/645):
a buildable image whose `/etc/cube-sandbox/tool-profile.txt` matches the host
argv gate in [`../code-sandbox-quickstart/tool_allowlist.py`](../code-sandbox-quickstart/tool_allowlist.py).

Same split many agent runtimes use in practice (E2B-style hosts, OpenAI
Agents `E2BSandboxClient`, etc.): **policy on the host**, **workload in the
sandbox**. Cube adds MicroVM isolation and `allow_internet_access=False`.

This directory is the template + smoke test. Gate unit tests and the
reference tool loop live under `code-sandbox-quickstart/` so the gate is not
tied to one image.

## Build

```bash
docker build -t agent-tool-allowlist-sandbox:latest .

# optional — only if you need guest `curl` for an egress demo
# docker build --build-arg INSTALL_CURL=1 -t agent-tool-allowlist-sandbox:latest .
```

Local smoke (envd probe), same idea as `cubesandbox-base-nginx`:

```bash
docker run --rm -d --name agent-tool-box \
  -p 49983:49983 agent-tool-allowlist-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f agent-tool-box
```

## Register template

```bash
cubemastercli tpl create-from-image \
  --image <registry-or-local>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

When the job is READY, put the template id in `.env` as `CUBE_TEMPLATE_ID`.

Resources: 1G writable layer is enough; no GPU.

## Configure & run

```bash
pip install -r requirements.txt
cp .env.example .env   # E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID

python verify_template.py
```

Expect: host deny for `bash`, then `tool-profile` + `echo`, `TEMPLATE_VERIFY_OK`.

Gate-only (no template build):

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

With this template registered:

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_allow.py
python tool_agent_loop.py
```

`tool_agent_loop.py` uses fixed proposals (not an LLM). The egress turn needs
a guest `curl` (from base or `INSTALL_CURL=1`); otherwise that turn is skipped.

## Limits

- Base image still has a shell; the profile file is intent, not confinement.
- Default allowlist + `echo … > file` can write guest paths — see quickstart
  threat model.
- Not a substitute for `sandbox-code` / interpreter templates.
- `create-from-image` needs an image reference the node can pull (push to a
  registry the cluster reaches; a bare `*:local` name is resolved via Docker Hub).
- Default build does not apt-install curl. The base image may still ship curl;
  that is inheritance, not part of `tool-profile.txt`. Use `INSTALL_CURL=1`
  only to pin curl into your own layer.

[中文说明](README_zh.md)
