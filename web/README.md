# CubeSandbox Dashboard (web/)

Modern, minimal, cool dashboard for CubeSandbox. Vite + React + TypeScript + Tailwind v3 + shadcn-style primitives + TanStack Query + cmdk (⌘K).

## Quickstart

```bash
make web-install
make web-dev     # → http://localhost:5173, proxies /cubeapi to CubeAPI @ :3000
make web-build   # → web/dist/
```

## Template-card layout regression

Guards the default `/templates` list-tab card grid (`Compatibility` and `Snapshots` tabs use separate layouts and are out of scope) against long unbroken aliases or template IDs widening `1fr` tracks, clipping status badges, or pushing badges underneath the top-right delete button. Overlong aliases and template IDs truncate with CSS ellipsis only when space runs out, keeping the full text in the DOM and in hover `title` attributes.

Run the WebUI locally, then execute the optional Playwright check (manual-only so CI does not pull Chromium on every `web/` change):

```bash
# Terminal 1, from web/
npm ci
npm run dev

# Terminal 2, from web/; use a disposable Python environment
python3 -m venv /tmp/cube-web-layout-venv
/tmp/cube-web-layout-venv/bin/pip install playwright
/tmp/cube-web-layout-venv/bin/playwright install chromium
/tmp/cube-web-layout-venv/bin/python scripts/test-template-card-layout.py
```

The script measures 50 card cases (`en`/`zh` × five widths `360–1440 px` × five templates covering `READY`, `RUNNING`, `FAILED`, `PARTIALLY_READY`, and `UNKNOWN` compatibility). It asserts badge/button card bounds and non-overlap, right-aligned wrapped badges on narrow cards, full ID text in the DOM with `title` tooltips, keyboard focus visibility and `aria-label` on the delete button, and an explicit check that opening the delete modal stays on `/templates` without issuing any `DELETE` request. Pass `--output <directory>` to write `results.json` and Light captures (`template-cards-<lang>-<360|1440>-light.png`), `--browser-executable <path>` to reuse a local Chromium, or `--baseline` against an unfixed checkout (e.g. `f2777699da7e`) to assert zero `fixture_errors` while requiring `layout_errors` to reproduce.

Request interception only mocks `fetch`/`xhr` calls for the four `GET` endpoints this page and shell issue (`/templates`, `/templates/compat`, `/opsapi/v1/auth/session`, `/opsapi/v1/cluster/versions`); document `/templates` navigations load `index.html`, static/module assets pass through, and any other `fetch`/`xhr` path or non-`GET` method is recorded in `fixture_errors` and completed with HTTP 500. This stays separate from `src/mocks` (`?mock=1`) because the MSW handlers target legacy `/cubeapi/v1/*` routes with `onUnhandledRequest: 'bypass'` and general-purpose sample data rather than strict layout edge cases.

## Routes

- `/` Overview — cluster KPIs, recent sandboxes, template pipeline
- `/sandboxes` list + lifecycle actions (pause/resume/kill)
- `/sandboxes/:id` detail + logs
- `/templates` catalog
- `/templates?tab=snapshots` snapshot list (paginated, delete)
- `/snapshots/:id` snapshot detail
- `/nodes` fleet health
- `/keys` store `X-API-Key` locally
- `/network`, `/observability`, `/settings` — roadmap placeholders

## Backend

Talks to **CubeAPI** (`CubeAPI/` in repo), which proxies templates/cluster/nodes to CubeMaster. See `src/api/client.ts`. Snapshot list, detail and delete are served by **CubeOps** (`:3010`).

The `X-API-Key` header (if present in localStorage under `cube.apiKey`) is injected on every request.

The dashboard always calls CubeAPI through the same-origin `/cubeapi/v1` prefix. In local development Vite proxies `/cubeapi` according to `vite.config.ts`; in one-click deployments a standard nginx container publishes port `12088`, mounts the packaged `webui/dist`, and proxies `/cubeapi` to the host CubeAPI through Docker `host-gateway`.
