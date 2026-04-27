# CubeSandbox Dashboard (web/)

Modern, minimal, cool dashboard for CubeSandbox. Vite + React + TypeScript + Tailwind v3 + shadcn-style primitives + TanStack Query + cmdk (⌘K).

## Quickstart

```bash
make web-install
make web-dev     # → http://localhost:5173, proxies /cubeapi to CubeAPI @ :3000
make web-build   # → web/dist/
```

## Routes

- `/` Overview — cluster KPIs, recent sandboxes, template pipeline
- `/sandboxes` list + lifecycle actions (pause/resume/kill)
- `/sandboxes/:id` detail + logs
- `/templates` catalog
- `/nodes` fleet health
- `/keys` store `X-API-Key` locally
- `/network`, `/observability`, `/settings` — roadmap placeholders

## Backend

Talks to **CubeAPI** (`CubeAPI/` in repo), which proxies templates/cluster/nodes to CubeMaster. See `src/api/client.ts`.

The `X-API-Key` header (if present in localStorage under `cube.apiKey`) is injected on every request.

The dashboard always calls CubeAPI through the same-origin `/cubeapi/v1` prefix. In local development Vite proxies `/cubeapi` according to `vite.config.ts`; in one-click deployments a standard nginx container publishes port `12088`, mounts the packaged `webui/dist`, and proxies `/cubeapi` to the host CubeAPI through Docker `host-gateway`.
