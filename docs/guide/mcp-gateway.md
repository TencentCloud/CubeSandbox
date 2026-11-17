# MCP in Sandboxes

::: warning Preview
MCP support is in preview. The `mcp` option and the example gateway may change.
:::

[MCP](https://modelcontextprotocol.io/) (Model Context Protocol) is how AI agents discover and call tools such as web search, file access, or a paper index. Each tool is provided by an **MCP server**, usually a small program that talks over stdio.

The E2B-compatible `mcp` option lets you ask for a set of MCP servers when you create a sandbox. The servers run **inside** the sandbox, isolated from your own machine. You get back one URL and one token, and your agent connects to that URL to see and call the tools of every server you asked for:

```python
from cubesandbox import Sandbox

sandbox = Sandbox.create(mcp={"time": {}, "fetch": {}})
url = sandbox.get_mcp_url()      # http://50005-<sandbox-id>.<domain>/mcp
token = sandbox.get_mcp_token()  # send as "Authorization: Bearer <token>"
```

The same code works with the stock E2B SDKs pointed at CubeAPI.

## How it works

```text
your agent ──(MCP over HTTP + token)──> 50005-<id>.<domain>/mcp
                                              │  CubeProxy
                                              ▼
                                    mcp-gateway in the sandbox
                                     ├── time server   (stdio)
                                     └── fetch server  (stdio)
```

1. **CubeAPI** checks the shape of `mcp` and rejects malformed input with `400` before any sandbox is created. It does not store the value, forward it to the scheduler, or return it.
2. **Template:** the SDK picks the template in the order described in [Template priority](#template-priority). **That template must contain an `mcp-gateway` program**; the default sandbox image does not.
3. **SDK:** after the sandbox starts, the SDK runs `mcp-gateway --config '<json>'` as `root` with a random token in `GATEWAY_ACCESS_TOKEN`. It waits up to 60 seconds, the same as the E2B SDK.
4. **Gateway:** it starts the requested servers and serves their tools on port `50005`. **CubeProxy** routes `50005-<id>.<domain>` to that port, just like any other sandbox port.
5. **Failure:** if the command fails or times out, the SDK kills the sandbox and raises `Failed to start MCP gateway: ...` with the gateway's output.

Cube itself only provides steps 1 and 3, plus routing. What runs inside the sandbox is decided by the template.

## Template priority

When a sandbox is created with `mcp`, the Cube SDKs choose its template in this order:

1. If `template` (`TemplateID` in Go) is passed, use that template.
2. If `template` is not passed, use the template with the alias `mcp-gateway`.
3. If no template with the alias `mcp-gateway` exists, use the template from the `CUBE_TEMPLATE_ID` environment variable.
4. If neither the `mcp-gateway` template nor the `CUBE_TEMPLATE_ID` template exists, creation fails with a template-not-found error.

> To change the alias `mcp` uses by default (`mcp-gateway`), set the `CUBE_MCP_TEMPLATE_ID` environment variable:
> ```bash
> export CUBE_MCP_TEMPLATE_ID=custom-mcp-gateway
> ```

## Gateway contract

Any program can serve as the gateway as long as it is installed as `mcp-gateway` on `PATH` and behaves like this:

| Item | Requirement |
| --- | --- |
| Command | `mcp-gateway --config '<json>'`, run as `root`. `<json>` is the `mcp` object from the create call. |
| Token | Read from the `GATEWAY_ACCESS_TOKEN` environment variable. Write it to `/etc/mcp-gateway/.token`, which is how `get_mcp_token()` recovers it after `Sandbox.connect`. |
| Readiness | Keep serving after the command exits, and exit `0` only once the endpoint is ready. Exit non-zero with a readable message on failure. Finish within 60 seconds. |
| Endpoint | MCP streamable HTTP on port `50005`, path `/mcp`. Reject requests without `Authorization: Bearer <token>`. |
| Template | Expose port `50005` when creating the template. |

## Quick start with the example gateway

The repository ships a reference implementation in [`examples/mcp-gateway`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mcp-gateway). It launches stdio MCP servers, merges their tools into one endpoint, and checks the token. Its template Dockerfile pre-installs a set of servers.

### 1. Build the template

```bash
cd examples/mcp-gateway
docker build -f template/Dockerfile \
  --build-arg BASE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
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

- `MCP_SERVERS` lists the servers to pre-install. Pre-install every server you plan to use: the gateway waits at most 55 seconds, and servers that are not pre-installed are downloaded on first start.
- `BASE_IMAGE` overrides the base image. The default, the CubeSandbox `sandbox-code` image, provides `uv`, `node`, and `git`.
- To use an alias other than `mcp-gateway`, pass `template` when creating sandboxes, or set `CUBE_MCP_TEMPLATE_ID` for the Cube SDKs. See [Template priority](#template-priority).

Wait until the template is `READY`.

### 2. Create a sandbox and call a tool

```python
import asyncio
from cubesandbox import Sandbox
from mcp import ClientSession                                    # pip install mcp
from mcp.client.streamable_http import streamablehttp_client

async def main():
    with Sandbox.create(mcp={"time": {}, "fetch": {}}) as sandbox:
        headers = {"Authorization": f"Bearer {sandbox.get_mcp_token()}"}
        async with streamablehttp_client(sandbox.get_mcp_url(), headers=headers) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                print([tool.name for tool in (await session.list_tools()).tools])
                result = await session.call_tool("get_current_time", {"timezone": "Asia/Shanghai"})
                print(result.content[0].text)

asyncio.run(main())
```

If `*.<domain>` does not resolve on your machine, send the request to CubeProxy directly and set the `Host` header to the sandbox host, the same way the SDKs do with `CUBE_PROXY_NODE_IP`.

The other SDKs expose the same calls:

::: code-group

```ts [Node.js]
const sandbox = await Sandbox.create({ mcp: { time: {}, fetch: {} } });
const url = sandbox.getMcpUrl();
const token = await sandbox.getMcpToken();
```

```go [Go]
sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{
    MCP: cubesandbox.MCPServers{"time": map[string]any{}},
})
url := sandbox.GetMCPURL()
token, err := sandbox.GetMCPToken(ctx)
```

:::

### Servers supported by the example gateway

`mcp` is an object keyed by server name. The example gateway understands three kinds of entries.

**Catalog servers.** The value holds the server's properties. Run `mcp-gateway catalog` in the template to list them.

| Name | Properties | Runs |
| --- | --- | --- |
| `duckduckgo` | | `uvx duckduckgo-mcp-server` |
| `arxiv` | `storagePath` (optional) | `uvx arxiv-mcp-server` |
| `fetch` | | `uvx mcp-server-fetch` |
| `time` | | `uvx mcp-server-time` |
| `filesystem` | `paths` (required, list) | `@modelcontextprotocol/server-filesystem` |
| `memory` | | `@modelcontextprotocol/server-memory` |
| `sequentialthinking` | | `@modelcontextprotocol/server-sequential-thinking` |

You can add or replace entries with `/etc/mcp-gateway/catalog.json` in your template. It uses the same format as `examples/mcp-gateway/internal/config/catalog.json`.

**GitHub servers.** These are keyed `github/<owner>/<repo>`, as in E2B. The gateway clones the repository, runs `installCmd` in it, and starts `runCmd`:

```python
Sandbox.create(mcp={
    "github/acme/weather-mcp": {
        "installCmd": "npm ci",
        "runCmd": "node dist/index.js",
        "envs": {"WEATHER_API_KEY": "..."},
    },
})
```

**Local commands.** Any other key with `runCmd` starts that command. Use this for servers already installed in your template.

When two servers expose tools with the same name, the first server by name keeps the plain name. Tools from later servers are renamed `<server>_<tool>`. Only tools are aggregated; MCP prompts and resources are not exposed. `GET /health` on port `50005` reports server and tool counts without a token.

## Input validation

CubeAPI returns `400` when:

- `mcp` is not an object;
- an entry is not an object;
- a GitHub key is not `github/<owner>/<repo>`;
- `runCmd` is missing or empty for a GitHub server;
- `envs` has an invalid name or a non-string value;
- there are more than 64 servers.

Whether a server name is known is up to the gateway. The example gateway fails to start on unknown names, and the SDK reports that error.

## Lifecycle

With the example gateway:

- **Pause / resume:** the gateway and its servers are part of the memory snapshot and keep serving with the same token after resume.
- **Connect:** `get_mcp_token()` reads the token file, so a new SDK handle can reuse the endpoint.
- **Clone and snapshot restore:** the running gateway and its token are copied too. Every clone accepts the same token, so treat clones as one trust domain.
- **Crashes:** if a server process exits, the gateway starts it again on the next call.

## Security notes

- The token protects the MCP endpoint only. CubeProxy authentication and network policies still apply, and servers inside the sandbox are subject to the sandbox's egress rules.
- **All MCP servers in a sandbox share one trust domain; they are not isolated from each other.** The example gateway runs every server as `root`, so any server can read the other servers' `envs` (for example through `/proc/<pid>/environ`) and `/etc/mcp-gateway/.token`, and send them elsewhere. The `installCmd` and `runCmd` of a GitHub server are arbitrary commands run as `root`. Do not put untrusted servers and sensitive credentials in the same sandbox; use a separate sandbox for each trust domain.
- As in the E2B contract, the SDKs pass the `mcp` config as a command-line argument (`mcp-gateway --config '<json>'`). While the gateway starts (up to about 60 seconds), the full config, including `envs`, is visible in that command's process arguments to any process in the sandbox.
- The example gateway removes the token from its own environment and from every MCP server's environment, and sets `envs` only in the environment of the server they belong to. It does not write them to disk or logs. This prevents accidental leaks, but does not protect against a malicious server as described above.
- The example gateway's log (`/var/log/mcp-gateway.log`) includes each server's `stderr`. A server that prints its own secrets will leak them there.

## Bring your own gateway

You can replace the example with any program that meets the [gateway contract](#gateway-contract). For example, you could put a thin launcher script named `mcp-gateway` in front of another gateway such as [Docker MCP Gateway](https://github.com/docker/mcp-gateway), which is what E2B's hosted `mcp-gateway` template is built on. Keep in mind that Docker MCP Gateway runs each server as a container, so the template would also need a working Docker daemon inside the sandbox. CubeSandbox has not validated that setup.
