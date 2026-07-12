# Ruby + Sinatra Sandbox Template

[中文文档](README_zh.md)

A reusable CubeSandbox template for Ruby web development. It installs Ruby,
Bundler, Sinatra, and Puma, serves a minimal stateful API on port `4567`, and
keeps Cube's `envd` readiness service on port `49983`.

## Use cases

- isolated execution of untrusted or generated Ruby applications
- reproducible Sinatra API development and testing
- stateful long-running web tasks with pause/resume checkpoints
- a small base for Rails, Hanami, Sidekiq, or Ruby agent runtimes

## Build and register

```bash
docker build -t ruby-sinatra-cube:latest examples/ruby-sinatra-sandbox
docker tag ruby-sinatra-cube:latest <registry>/ruby-sinatra-cube:latest
docker push <registry>/ruby-sinatra-cube:latest

cubemastercli tpl create-from-image \
  --image <registry>/ruby-sinatra-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 4567 \
  --probe 49983 \
  --probe-path /health
```

The Cube probe targets inherited `envd`, while application traffic uses port
`4567`. Pinning gem versions in `Gemfile` keeps rebuilds predictable.

## Run the example

```bash
cd examples/ruby-sinatra-sandbox
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
# Set E2B_API_URL and CUBE_TEMPLATE_ID in .env.
python run_example.py
```

The script creates a sandbox, waits for `/health`, increments the persistent
counter, and prints the CubeProxy URL.

## Checkpoint and resume

```bash
python resume_example.py
```

This writes `41` to `/workspace/data/counter.txt`, pauses the MicroVM, reconnects
to the same sandbox, and verifies the file survived. Keep mutable application
data under `/workspace`; bake dependencies into the image.

## Security and resources

- Recommended minimum: 1 vCPU, 512 MiB RAM, 2 GiB writable layer.
- The application does not need outbound network at runtime. For untrusted Ruby
  code, create it with `allow_internet_access=False` or an explicit allowlist.
- Do not bake credentials into the image. Pass short-lived values with
  `Sandbox.create(envs=...)`; use CubeEgress injection for high-value secrets.
- Keep port `49983` for `envd`; expose only application ports users need.

## Known limitations and troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| Template probe times out | Probe `49983/health`, not the Sinatra port |
| `502` on port 4567 | Wait for Puma startup; inspect `sandbox.commands.run("ps aux")` |
| TLS verification error | Set `REQUESTS_CA_BUNDLE` to the deployment CA |
| `bundle install` fails | Permit `rubygems.org` during image build or use an internal mirror |
| Native gem build fails | Add its system headers to the Dockerfile build layer |
| Pause/resume unavailable | Upgrade to CubeSandbox `>= 0.3.0` |

The Docker image can also be smoke-tested locally with ports `4567` and `49983`
published before it is registered as a template.
