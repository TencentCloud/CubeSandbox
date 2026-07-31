---
title: WebUI Dashboard
---

# WebUI Dashboard

The Cube Sandbox **Dashboard** is a built-in web console that lets you see what's running, manage sandboxes, build templates, and inspect cluster health — all from your browser, no CLI required.

> ⏱ Takes ~3 minutes to read. After that you can drive a cluster from a laptop.

## 1. Where do I open it?

The Dashboard is a static frontend served by an nginx container on the **control node**.

| Scenario | URL | Notes |
| --- | --- | --- |
| One-click / multi-node deploy | `http://<control-node-ip>:12088` | Default port, change via `WEB_UI_HOST_PORT` |
| Bare-metal deploy | `http://<server-ip>:12088` | Same port |
| Local development | `http://localhost:5173` | Vite dev server, proxies `/cubeapi` to `127.0.0.1:3000` |

::: tip Port 12088 vs CubeOps :3010
Port `12088` is the human-facing Dashboard (nginx). Behind it, **CubeOps** (the ops/admin service) listens on `:3010`. The Dashboard talks to CubeOps under two same-origin prefixes:
- `/opsapi/*` → CubeOps `/api/*` (admin endpoints, **restricted to localhost and Docker bridge networks**)
- `/cubeapi/v1/*` → CubeOps `/api/v1/sdk/*` (E2B-compatible SDK endpoints, JWT-authenticated, public)

You only ever need to open `12088` from your browser. Do **not** expose `:3010` directly to the public internet.
:::

If you don't know your control-node IP, run `ip -4 addr` on the server, or check `http://<hostname>:12088` on the same LAN.

## 2. The sidebar at a glance

Everything lives behind the 11 icons in the left rail. Hover any icon to see its name.

| # | Icon | Page | What it's for |
| --- | --- | --- | --- |
| 1 | 📊 | **Overview** | Cluster KPIs: running sandboxes, CPU/memory usage, healthy nodes |
| 2 | 📦 | **Sandboxes** | Live list of every micro-VM, with pause / resume / kill actions |
| 3 | 🧩 | **Templates** | Catalog of reusable sandbox snapshots; create new ones from OCI images |
| 4 | 🖥️ | **Nodes** | Fleet health: per-host CPU, memory, slot capacity |
| 5 | 🧬 | **Versions** | Component version matrix across nodes (kernel, agent, guest image) |
| 6 | 🌐 | **Network** | API gateway config and per-node rate limits |
| 7 | 📈 | **Observability** | Runtime status, sandbox health, template build overview |
| 8 | 🔑 | **API Keys** | SDK API key management (JWT-based since v0.6.0) |
| 9 | 🏪 | **Template Store** | Install official preset images to bootstrap templates |
| 10 | 🤖 | **AgentHub** | Recruit and manage AI agent instances running on Cube Sandbox |
| 11 | ⚙️ | **Settings** | Theme, language, cluster info, keyboard shortcuts |

::: tip New user? Start with **Overview**.
It shows everything important in one screen and refreshes automatically.
:::

## 3. Three things you'll do first

### 3.1 Check that the cluster is healthy

Open **Overview** (`/`). You should see four green-ish KPI cards:

- **Running Sandboxes** — how many micro-VMs are live
- **CPU / Memory Utilization** — cluster-wide pressure
- **Healthy Nodes** — `N/M` nodes reporting `Ready`

If any number is red, click into **Nodes** to see which host is unhappy.

### 3.2 Create a sandbox

1. Click **Sandboxes** in the left rail, then **+ New sandbox** (top-right).
2. Pick a template from the grid. Templates marked `STALE` are disabled — pick a `READY` one.
3. (Optional) Add a few `meta` key/value pairs as labels.
4. Click **Create**. Within a couple of seconds you'll be redirected to the sandbox's detail page, where you can watch its logs stream in real time.

To stop a sandbox, go to **Sandboxes**, find the row, and click the pause / kill button on the right.

### 3.3 Log in (JWT authentication)

The Dashboard uses **JWT-based authentication** (since v0.6.0, replacing the old `X-API-Key` scheme). On first visit you'll be redirected to the login page.

1. Enter your credentials. The default All-in-One account is `admin` / `admin` — **change this immediately in production** via Settings → Change Password.
2. On success you receive an access token (short-lived) and a refresh token (7 days). Tokens are stored in `localStorage` and sent as `Authorization: Bearer <jwt>`.
3. The admin endpoints (`/opsapi/*`) are **restricted to localhost and Docker bridge networks** at the nginx layer, so even with weak default credentials they are not reachable from the public internet. SDK endpoints (`/cubeapi/v1/*`) require a valid JWT.

::: details Token lifecycle
- **Access token**: 15 min TTL, `token_type=access`, audience `cubeops:access`.
- **Refresh token**: 7 day TTL, `token_type=refresh`, audience `cubeops:refresh`. Refresh tokens **cannot** be used as access tokens (enforced by `typ` + `aud` claims).
- Login is rate-limited: 5 failed attempts per minute per IP.
:::

## 4. Open a Web Terminal

The Web Terminal gives an authenticated Dashboard user an interactive shell in
a **running** sandbox container. It uses the same envd PTY data plane as the
CubeSandbox SDK; it does not grant host access or bypass the sandbox's network
and egress policy.

### Before you start

- Sign in to the Dashboard. Terminal ticket creation is protected by the same
  CubeOps JWT authentication as other sandbox operations.
- Start a sandbox and wait until its state is **Running**. The terminal button
  is disabled for paused, pausing, stopped, or missing sandboxes.
- The selected container image must include envd. New multi-container
  sandboxes expose an independent envd endpoint for each container.
- Use HTTPS for production deployments. The browser automatically changes the
  terminal channel from `ws://` to encrypted `wss://`.

### Start a shell

1. Open **Sandboxes**.
2. Select **Open terminal** in a running sandbox's action column, or open the
   sandbox detail page and select **Open terminal** there.
3. If the sandbox contains more than one terminal-capable container, select
   the target from the **Container** menu.
4. Wait for the green **Connected** status, then run:

   ```sh
   ls
   top
   ping -c 3 127.0.0.1
   ```

   Press `q` to exit `top`. ANSI color, cursor control, scrollback, resize, and
   normal browser copy/paste shortcuts are supported.
5. Select **Disconnect** to actively terminate the shell. Closing the panel
   also terminates an active session.

The toolbar can enter fullscreen and change the terminal font size. Each panel
opens an independent PTY, so multiple users, browser tabs, sandboxes, and
containers can be used concurrently within the configured session limits.

### Disconnects, timeout, and audit

CubeOps sends WebSocket keepalives. If the network drops unexpectedly, the
panel shows **Connection lost** and offers **Reconnect**; the PTY is retained
for 30 seconds by default. An inactive session is closed after 15 minutes by
default. Operators can tune these values with:

```sh
CUBE_OPS_TERMINAL_IDLE_TIMEOUT=15m
CUBE_OPS_TERMINAL_RECONNECT_GRACE=30s
CUBE_OPS_TERMINAL_MAX_SESSIONS=128
CUBE_OPS_TERMINAL_MAX_SESSIONS_PER_SANDBOX=8
```

For a non-default topology, also set `CUBE_SANDBOX_PROXY_URL` to the internal
HTTP endpoint of CubeProxy (for example, `http://cube-proxy:80`). CubeOps keeps
the virtual sandbox host name while dialing that endpoint.

CubeOps writes structured `terminal audit` entries when a ticket is issued and
when a session opens or closes. The log includes operator, timestamp, sandbox,
container, session, and close reason.

### Security and known limits

- A WebSocket URL carries only a random, single-use ticket that expires after
  30 seconds—not the user's JWT. Same-origin checks apply to browser upgrades.
- Commands run with the container's existing envd identity and remain inside
  its isolation, filesystem, Linux capabilities, and egress policy.
- Sandboxes created before per-container envd metadata was introduced can use
  the primary container on port `49983`; an older sidecar without endpoint
  metadata is shown as unavailable.
- The session starts `/bin/sh -i` for compatibility with minimal images.
  Commands absent from the image (for example `ping`) must be installed in the
  image using its normal package policy.
- Reconnection is available only during the configured grace window and only
  to the same authenticated operator, sandbox, and container.

## 5. Keyboard shortcuts

The Dashboard is keyboard-friendly. The big three:

| Key | Action |
| --- | --- |
| `⌘ K` / `Ctrl K` | Open the **Command Palette** — type a page name to jump there |
| `?` | Open **Settings → Shortcuts** (this list, but in-app) |
| `R` | Refetch every visible data panel |
| `Esc` | Close any open modal or the Command Palette |

## 6. Personalize it

Open **Settings** in the left rail:

- **Appearance → Theme** — Light, Dark, or follow your OS
- **Appearance → Language** — English or 简体中文
- **Cluster** — Read-only view of the CubeAPI endpoint, sandbox domain, default instance type, rate limit, and whether auth is on

The Command Palette's ⌘K input box and the topbar have quick toggles for the same.

## 7. FAQ

**Why a separate Dashboard, not just curl?**
Most operations (create-from-image, version matrix, node triage) are easier to discover and visualize in a UI. For automation, the Dashboard is just a thin client — every page is a call to `/cubeapi/v1/*`, which is the same E2B-compatible REST API you can hit with `curl` or the E2B SDK.

**Does the Dashboard store my data?**
It stores only one thing in your browser: the API key under `localStorage.cube.apiKey`. All other state (templates, sandboxes, logs) lives on the cluster.

**Can I change the port?**
Yes — set `WEB_UI_HOST_PORT` in `.env` before running `install.sh`. The change applies on next start of `cube-sandbox-webui.service`.

**Can I disable the Dashboard?**
Yes — set `WEB_UI_ENABLE=0` (or unset) in `.env`. The cluster keeps running; you just won't have the web UI. The E2B-compatible API on port `3000` is unaffected.

**Is the Dashboard open source? Can I run my own build?**
Yes — it lives in `web/` of the repo, built with Vite + React + TypeScript + Tailwind. See [Self-Build Deployment](./self-build-deploy.md) and the [`web/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/web/README.md) for details.

## 8. Next steps

- [Quick Start](./quickstart.md) — if you haven't installed yet, get to a running Dashboard in minutes
- [Service Management](./service-management.md) — how to start/stop/restart the `cube-sandbox-webui.service` container
- [Authentication](./authentication.md) — turn on API keys if you haven't
- [HTTPS & Domain Resolution](./https-and-domain.md) — put the Dashboard behind TLS
- [Architecture Overview](../architecture/overview.md) — understand how CubeAPI, CubeMaster, Cubelet fit together behind the scenes
