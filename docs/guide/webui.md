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

::: tip Port 12088, not 3000
Port `3000` is the E2B-compatible REST API (CubeAPI). Port `12088` is the human-facing Dashboard. The Dashboard internally calls CubeAPI under the same-origin prefix `/cubeapi/v1`, so you only ever need to open `12088`.
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
| 8 | 🔑 | **API Keys** | Store the `X-API-Key` value used for all Dashboard requests |
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

### 3.3 Configure the API key (only if auth is enabled)

If your deployment has authentication turned on, the Dashboard needs an API key before any request will succeed.

1. Open **API Keys** in the left rail.
2. Paste your key (it looks like `sk-cube-…`) into the input.
3. Click **Save**. The value is stored in your browser's `localStorage` under `cube.apiKey` and attached to every Dashboard request as the `X-API-Key` header.

::: details Where does the key come from?
The admin who enabled auth generated it. See [Authentication](./authentication.md) for the full flow.
:::

## 4. Keyboard shortcuts

The Dashboard is keyboard-friendly. The big three:

| Key | Action |
| --- | --- |
| `⌘ K` / `Ctrl K` | Open the **Command Palette** — type a page name to jump there |
| `?` | Open **Settings → Shortcuts** (this list, but in-app) |
| `R` | Refetch every visible data panel |
| `Esc` | Close any open modal or the Command Palette |

## 5. Personalize it

Open **Settings** in the left rail:

- **Appearance → Theme** — Light, Dark, or follow your OS
- **Appearance → Language** — English or 简体中文
- **Cluster** — Read-only view of the CubeAPI endpoint, sandbox domain, default instance type, rate limit, and whether auth is on

The Command Palette's ⌘K input box and the topbar have quick toggles for the same.

## 6. FAQ

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

## 7. Next steps

- [Quick Start](./quickstart.md) — if you haven't installed yet, get to a running Dashboard in minutes
- [Service Management](./service-management.md) — how to start/stop/restart the `cube-sandbox-webui.service` container
- [Authentication](./authentication.md) — turn on API keys if you haven't
- [HTTPS & Domain Resolution](./https-and-domain.md) — put the Dashboard behind TLS
- [Architecture Overview](../architecture/overview.md) — understand how CubeAPI, CubeMaster, Cubelet fit together behind the scenes
