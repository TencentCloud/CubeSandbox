# CubeOps

CubeOps is the operations backend for the CubeSandbox platform. It provides
the admin WebUI API surface (agent management, cluster monitoring, store
metadata, authentication) and is designed to run **inside the internal
network only** — never exposed to the public internet.

## Architecture

CubeOps is the "ops half" of the CubeAPI/CubeOps split:

- **CubeAPI** (Rust/Axum) — stateless, public-facing, E2B-compatible SDK API
- **CubeOps** (Go) — stateful, internal-only, admin/ops API

Both services share the same MySQL database. Schema migrations are managed
by the shared [`cubedb`](../cubedb) Go module, which wraps goose with
content-fingerprint tamper detection and cluster-wide locking.

## Quick Start

```bash
# 1. Set required environment variables
export JWT_SECRET=$(openssl rand -hex 32)
export DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
export CUBE_MASTER_ADDR=http://127.0.0.1:8089

# 2. Build and run
make run
```

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_OPS_BIND` | `127.0.0.1:3010` | Listen address (internal only) |
| `CUBE_OPS_LOG_LEVEL` | `info` | Log level |
| `JWT_SECRET` | *(required)* | JWT signing secret (32+ bytes) |
| `JWT_ACCESS_TTL` | `15m` | Access token TTL |
| `JWT_REFRESH_TTL` | `168h` | Refresh token TTL (7 days) |
| `DATABASE_URL` | *(required)* | MySQL connection URL |
| `CUBE_MASTER_ADDR` | `http://127.0.0.1:8089` | CubeMaster base URL |
| `REDIS_URL` | *(optional)* | Redis for JWT blacklist |

## Authentication

CubeOps uses JWT-based authentication:

1. `POST /api/v1/auth/login` → returns `{ accessToken, refreshToken }`
2. Subsequent requests carry `Authorization: Bearer <accessToken>`
3. When the access token expires, `POST /api/v1/auth/refresh` with the refresh token
4. Default admin account: `admin` / `admin` (change after first login)

RBAC is reserved for future use — currently any valid JWT grants full access.

## API Endpoints

### Auth
- `POST /api/v1/auth/login` — Login
- `POST /api/v1/auth/refresh` — Refresh access token
- `GET /api/v1/auth/session` — Check session
- `POST /api/v1/auth/logout` — Logout
- `POST /api/v1/auth/change-password` — Change password

### Cluster
- `GET /api/v1/cluster/overview` — Cluster capacity overview
- `GET /api/v1/cluster/versions` — Component version matrix
- `GET /api/v1/nodes` — Node list
- `GET /api/v1/nodes/{nodeID}` — Node detail

### AgentHub
- `GET /api/v1/agenthub/instances` — List agent instances
- `POST /api/v1/agenthub/instances` — Create agent instance
- `DELETE /api/v1/agenthub/instances/{agentID}` — Delete agent instance
- `POST /api/v1/agenthub/instances/{agentID}/restart` — Restart agent
- `POST /api/v1/agenthub/instances/{agentID}/pause` — Pause agent
- `POST /api/v1/agenthub/instances/{agentID}/resume` — Resume agent
- `POST /api/v1/agenthub/instances/{agentID}/upgrade` — Upgrade agent
- `PUT /api/v1/agenthub/instances/{agentID}/model` — Update model
- `GET|PUT /api/v1/agenthub/instances/{agentID}/wecom` — WeCom config
- `GET /api/v1/agenthub/instances/{agentID}/operations` — Operations log
- `GET /api/v1/agenthub/instances/{agentID}/gateway/health` — Gateway health
- `GET|POST /api/v1/agenthub/instances/{agentID}/snapshots` — Snapshots
- `DELETE|PATCH /api/v1/agenthub/instances/{agentID}/snapshots/{snapshotID}` — Snapshot ops
- `POST /api/v1/agenthub/instances/{agentID}/rollback` — Rollback
- `POST /api/v1/agenthub/instances/{agentID}/recover` — Recover
- `POST /api/v1/agenthub/instances/{agentID}/clone` — Clone
- `POST /api/v1/agenthub/instances/{agentID}/publish-template` — Publish template
- `GET /api/v1/agenthub/templates` — List templates
- `POST /api/v1/agenthub/templates/market` — Register market template
- `PATCH|DELETE /api/v1/agenthub/templates/{templateID}` — Template ops
- `GET|PUT /api/v1/agenthub/settings` — Global settings

### Store
- `GET /api/v1/store/meta` — Image metadata
- `POST /api/v1/store/refresh` — Pull + refresh images

### Config
- `GET /api/v1/config` — Runtime config snapshot

## Development

```bash
# Build
make build

# Test
make test

# Format
make fmt

# Docker
make docker
```

## Dependencies

- [cubedb](../cubedb) — Shared database migration & DAO package
- [CubeMaster](../CubeMaster) — Cluster orchestrator (HTTP API)
- MySQL 8.0 — Shared database
- Docker — For store image metadata (docker inspect/pull)
