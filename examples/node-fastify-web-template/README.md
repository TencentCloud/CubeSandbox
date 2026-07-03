# Node.js / Fastify Web Template

[中文文档](README_zh.md)

This example shows how to build a CubeSandbox-ready Node.js web development sandbox template. It runs CubeSandbox envd for SDK access and a TypeScript Fastify API as the user web service.

## What it demonstrates

- Custom Node.js web development sandbox template
- Fastify + TypeScript API running on port `3000`
- CubeSandbox envd integration on port `49983`
- Stateful workspace data under `/workspace/state`
- Snapshot / resume behavior through the CubeSandbox-compatible E2B SDK
- Docker-based, reproducible sandbox runtime

## Tech stack

| Component | Choice |
|-----------|--------|
| Base image | `node:24-bookworm-slim` |
| CubeSandbox envd source | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| Web framework | Fastify |
| Language | TypeScript |
| Local dev runner | `tsx` |
| Build | `tsc` |
| Runtime command | `node dist/server.js` |

## Local development

```bash
npm install
npm run dev
```

Build and run the compiled server:

```bash
npm run build
npm start
```

Run the test suite:

```bash
npm test
```

The tests cover state-file corruption, schema validation failures, malformed JSON, and a real HTTP listener on an ephemeral port.

The service listens on `0.0.0.0:3000` by default. Set `PORT` to change the web API port, and `STATE_DIR` to change the state directory.

Useful endpoints:

| Endpoint | Description |
|----------|-------------|
| `GET /` | HTML landing page |
| `GET /health` | Readiness check |
| `GET /api/info` | Runtime information |
| `POST /api/counter` | Increment `/workspace/state/counter.json` |
| `POST /api/write-note` | Append a note to `/workspace/state/notes.jsonl` |

## Docker build and local verification

```bash
docker build -t cube-node-fastify-web:latest .

docker run --rm -d \
  -p 49983:49983 \
  -p 3000:3000 \
  --name cube-node-fastify-web \
  cube-node-fastify-web:latest

curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

curl -s http://127.0.0.1:3000/health

docker rm -f cube-node-fastify-web
```

Port `49983` is for CubeSandbox envd and SDK operations. Port `3000` is the Fastify web service exposed by this template.

## Create a CubeSandbox template

Push the image to a registry reachable by CubeSandbox nodes, then create a template:

```bash
docker tag cube-node-fastify-web:latest <your-registry>/cube-node-fastify-web:latest
docker push <your-registry>/cube-node-fastify-web:latest

cubemastercli -a 127.0.0.1 -p 8089 tpl create-from-image \
  --image <your-registry>/cube-node-fastify-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 49983 \
  --probe-path /health
```

Copy the template ID into `.env` as `CUBE_TEMPLATE_ID`.

## Run the Python demos

```bash
pip install -r requirements.txt
cp .env.example .env
```

Edit `.env`:

| Variable | Description |
|----------|-------------|
| `E2B_API_KEY` | CubeSandbox / E2B-compatible API key |
| `E2B_API_URL` | CubeAPI URL, for example `http://127.0.0.1:8089` |
| `CUBE_TEMPLATE_ID` | Template ID created from this Docker image |

Run the basic web API demo:

```bash
python run_demo.py
```

Run the snapshot / resume demo:

```bash
python snapshot_resume.py
```

The snapshot / resume demo increments the counter, pauses the sandbox, reconnects to the same sandbox, waits for Fastify readiness again, and increments the counter one more time. The counter file is stored under `/workspace/state`, so the value should continue after resume.

## Resource recommendations

- Use a writable layer of `1G` or more.
- This template is suitable for lightweight web APIs, mock backends, and agent tool servers.
- Increase CPU and memory when adding build tools, databases, or heavier application dependencies.

## Known limitations

- This is a demo template, not a production-hardened Node.js image.
- Keeping `node_modules` inside the image increases image size.
- The image registry must be reachable from CubeSandbox nodes.
- After resume, wait for the web service readiness endpoint before sending application requests.
