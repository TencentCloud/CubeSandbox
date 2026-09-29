# mcp-gateway example

[中文](README_zh.md)

A reference `mcp-gateway` for the E2B-compatible `mcp` sandbox option, plus a Dockerfile that turns it into a sandbox template. The SDKs only run `mcp-gateway --config '<json>'` inside the sandbox; any program that follows the [gateway contract](../../docs/guide/mcp-gateway.md#gateway-contract) can take its place. This one is provided so you can try the feature end to end and adapt it to your own templates.

It starts the requested MCP servers over stdio and serves all of their tools from one bearer-protected streamable HTTP endpoint on port `50005` (`/mcp`). For the user guide, see [docs/guide/mcp-gateway.md](../../docs/guide/mcp-gateway.md).

## Build a template

```bash
cd examples/mcp-gateway
docker build -f template/Dockerfile \
  --build-arg MCP_SERVERS="duckduckgo fetch time arxiv" \
  -t <registry>/mcp-gateway:latest .
docker push <registry>/mcp-gateway:latest

cubemastercli tpl create-from-image \
  --image <registry>/mcp-gateway:latest \
  --alias mcp-gateway \
  --writable-layer-size 1G \
  --expose-port 49983 --expose-port 49999 --expose-port 50005 \
  --probe 49983 --probe-path /health
```

Build arguments:

- `BASE_IMAGE`: base image. The default is the CubeSandbox `sandbox-code` image, which provides `uv`, `node`, and `git`.
- `MCP_SERVERS`: catalog servers to pre-install. Pre-install every server you plan to use: the SDK waits 60 seconds for the gateway, and servers that are not pre-installed are downloaded on first start.

## Commands

| Command | Purpose |
| --- | --- |
| `mcp-gateway --config '<json>'` | Validate the config, start the gateway in the background, and wait until every server is ready (55 seconds by default; change it with `--startup-timeout`). This is what the SDKs run after create. |
| `mcp-gateway serve` | Run the gateway in the foreground. |
| `mcp-gateway pull <server>...` | Pre-install catalog servers while building a template. |
| `mcp-gateway catalog` | List the built-in and template-provided servers. |

The launcher reads the token from `GATEWAY_ACCESS_TOKEN`, or generates one if it is unset. It writes the token to `/etc/mcp-gateway/.token` (mode `0600`) and passes the config and token to the background process over stdin. The config is never written to disk, and neither value appears in the background process's arguments or environment. The config does appear in the launcher's own arguments until it exits, because that is how the SDKs pass it (`--config '<json>'`).

All MCP servers run as `root` and are not isolated from each other: any server can read the other servers' `envs` and the token file. Treat every server in a sandbox as trusted; see the [security notes](../../docs/guide/mcp-gateway.md#security-notes).

## Layout

- `cmd/mcp-gateway`: entry point.
- `internal/cli`: launcher, `serve`, `pull`, `catalog`.
- `internal/config`: `mcp` parsing and the built-in catalog (`catalog.json`). Templates can extend the catalog with `/etc/mcp-gateway/catalog.json`.
- `internal/gateway`: upstream management, tool aggregation, and bearer authentication. Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).
- `template/Dockerfile`: builds the `mcp-gateway` sandbox template.

## Build and test

Requires Go 1.25 or later.

```bash
cd examples/mcp-gateway
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o mcp-gateway ./cmd/mcp-gateway
```
