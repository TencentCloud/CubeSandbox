# CubeOps

CubeOps is the operations backend for the CubeSandbox platform. It provides
the admin WebUI API surface (agent management, cluster monitoring, store
metadata, authentication).

## Architecture

CubeOps is the "ops half" of the CubeAPI/CubeOps split:

- **CubeAPI** (Rust/Axum) — stateless, public-facing, E2B-compatible SDK API (no DB)
- **CubeOps** (Go) — stateful admin/ops API + SDK proxy to CubeMaster. Listens on `:3010`; in All-in-One mode binds `0.0.0.0:3010` so the WebUI nginx container can reach it via `host.docker.internal`. Change the default password in production.

Both services share the same MySQL database. Schema migrations are managed
by the shared [`CubeDB`](../CubeDB) Go module, which wraps goose with
content-fingerprint tamper detection and cluster-wide locking.

CubeOps exposes two API groups:

1. **Admin/Ops API** (`/api/v1/auth`, `/api/v1/cluster`, `/api/v1/agenthub`,
   `/api/v1/store`, `/api/v1/config`) — used by the WebUI for cluster
   management, digital assistant (AgentHub) lifecycle, and store operations.
2. **SDK API** (`/api/v1/sdk/*`) — used by the WebUI for sandbox/template/
   snapshot CRUD. These endpoints call CubeMaster HTTP REST API directly
   (replacing the former CubeAPI reverse proxy).

## Quick Start

```bash
# 1. Set required environment variables
export DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
export CUBE_MASTER_ADDR=http://127.0.0.1:8089

# 2. Build and run
make run
```

`make run` builds the binary (`make build`) and starts it with hardcoded
defaults. Note that the Makefile sets `JWT_SECRET=test-secret-dummy` at the
command level, which overrides any `export JWT_SECRET=...` in your shell.
For a real deployment, run the binary directly with your own env:

```bash
make build
export JWT_SECRET=$(openssl rand -hex 32)
./bin/cubeops
```

> **Migration fingerprint check**: if you are connecting to a database that
> was previously migrated by an older version of the codebase, you may see
> a `migration fingerprint check failed` error on startup. This is a safety
> guard against silent schema drift. To bypass it (e.g. in dev), set
> `CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK=1`. The one-click deployment
> sets this automatically in `.one-click.env`.

## Service Management

In one-click deployments CubeOps is managed by systemd as
`cube-sandbox-cubeops.service`:

```bash
# Check status (shows PID, memory, recent logs)
systemctl status cube-sandbox-cubeops.service

# For scripts: returns "active" / "inactive" / "failed" (exit code 0/1/2)
systemctl is-active cube-sandbox-cubeops.service

# Start / stop / restart
systemctl start cube-sandbox-cubeops.service
systemctl stop cube-sandbox-cubeops.service
systemctl restart cube-sandbox-cubeops.service

# View recent logs
journalctl -u cube-sandbox-cubeops.service -n 50 --no-pager

# Follow logs in real-time
journalctl -u cube-sandbox-cubeops.service -f
```

Quick health check (no auth required):

```bash
curl -s http://127.0.0.1:3010/health
# → ok
```

The systemd unit reads environment variables from `.one-click.env` via the
start script at `deploy/one-click/scripts/systemd/cubeops-start.sh`.

## Configuration

CubeOps supports two configuration methods, which can be combined:

### Option 1: YAML config file (recommended)

Copy the example and edit:

```bash
cp config.example.yaml /etc/cube/ops.yaml
vi /etc/cube/ops.yaml
```

Or point to a custom path:

```bash
export CUBE_OPS_CONFIG=/path/to/your/config.yaml
```

See [`config.example.yaml`](./config.example.yaml) for all available fields
with inline comments.

### Option 2: Environment variables (legacy, still supported)

Environment variables take precedence over YAML — use this to override
individual fields without editing the YAML file.

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_OPS_BIND` | `127.0.0.1:3010` | Listen address. In All-in-One deployments this must be set to `0.0.0.0:3010` so the WebUI nginx container can reach CubeOps via `host.docker.internal:3010`. |
| `CUBE_OPS_LOG_LEVEL` | `info` | Log level |
| `JWT_SECRET` | *(optional)* | JWT signing secret. If unset, a secret is auto-generated on first startup and persisted to the `t_system_setting` table in the database. |
| `JWT_ACCESS_TTL` | `15m` | Access token TTL |
| `JWT_REFRESH_TTL` | `168h` | Refresh token TTL (7 days) |
| `DATABASE_URL` | *(required)* | MySQL connection URL. If unset, built from `CUBE_SANDBOX_MYSQL_{HOST,PORT,USER,PASSWORD,DB}` env vars. |
| `CUBE_MASTER_ADDR` | `http://127.0.0.1:8089` | CubeMaster base URL |
| `CUBE_API_SANDBOX_DOMAIN` | `cube.app` | Sandbox domain (used by SDK handler for sandbox URL construction) |
| `REDIS_URL` | *(optional)* | Redis for JWT blacklist |

**Resolution order**: environment variables > YAML file > built-in defaults.

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
- `GET /api/v1/store/meta` — Image metadata (inspects locally cached Docker images via `docker image inspect`)
- `POST /api/v1/store/refresh` — Pull + refresh images (`docker pull` then inspect)

### Config
- `GET /api/v1/config` — Runtime config snapshot

### SDK (WebUI sandbox/template/snapshot operations via CubeMaster direct)

These endpoints replace the former CubeAPI reverse proxy; CubeOps calls
CubeMaster HTTP REST API directly for all SDK data needs. The WebUI frontend
uses these as its primary data path.

**Sandboxes**
- `GET /api/v1/sdk/sandboxes` — List sandboxes
- `POST /api/v1/sdk/sandboxes` — Create sandbox
- `GET /api/v1/sdk/sandboxes/{id}` — Get sandbox detail
- `DELETE /api/v1/sdk/sandboxes/{id}` — Delete (kill) sandbox
- `GET /api/v1/sdk/sandboxes/{id}/logs` — Sandbox logs
- `POST /api/v1/sdk/sandboxes/{id}/timeout` — Set sandbox timeout
- `POST /api/v1/sdk/sandboxes/{id}/refreshes` — Refresh sandbox
- `POST /api/v1/sdk/sandboxes/{id}/pause` — Pause sandbox
- `POST /api/v1/sdk/sandboxes/{id}/resume` — Resume sandbox
- `POST /api/v1/sdk/sandboxes/{id}/connect` — Connect to existing sandbox

**V2 Sandboxes (E2B v2 compatible)**
- `GET /api/v1/sdk/v2/sandboxes` — List sandboxes (v2 format)
- `GET /api/v1/sdk/v2/sandboxes/{id}/logs` — Sandbox logs (v2 format)

**Snapshots**
- `GET /api/v1/sdk/snapshots` — List snapshots
- `POST /api/v1/sdk/sandboxes/{id}/snapshots` — Create snapshot
- `POST /api/v1/sdk/sandboxes/{id}/rollback` — Rollback sandbox to snapshot

**Templates**
- `GET /api/v1/sdk/templates` — List templates
- `POST /api/v1/sdk/templates` — Create template
- `GET /api/v1/sdk/templates/compat` — Template compatibility matrix
- `GET /api/v1/sdk/templates/{id}` — Get template detail
- `POST /api/v1/sdk/templates/{id}` — Rebuild template
- `DELETE /api/v1/sdk/templates/{id}` — Delete template
- `POST /api/v1/sdk/templates/{id}/builds/{buildID}` — Start template build
- `GET /api/v1/sdk/templates/{id}/builds/{buildID}/status` — Template build status
- `GET /api/v1/sdk/templates/{id}/builds/{buildID}/logs` — Template build logs

## Development

```bash
# Build
make build

# Run (sets JWT_SECRET=test-secret-dummy at the command level)
make run

# Format
make fmt

# Docker
make docker
```

## Testing

CubeOps has three levels of tests: unit tests (no external dependencies),
HTTP handler tests (fake CubeMaster client + real gin router), and
integration tests (Docker MySQL + real database).

### Run all tests

```bash
# All tests (unit + handler + integration). Integration tests spin up
# throwaway MySQL containers via dockertest; they auto-skip with t.Skip()
# when the Docker daemon is unavailable, so this command is safe to run
# with or without Docker installed.
go test ./... -timeout 600s
```

`-timeout` only sets the upper time bound for the whole test binary (the
default is 10 minutes); it does **not** select which tests run. Whether
integration tests execute is decided at runtime by `dockertest.NewPool`:
Docker reachable → run; Docker missing → `t.Skipf`.

If Docker is unavailable and you want the missing-Docker condition to be a
hard failure instead of a silent skip (e.g. in CI), set
`CUBEOPS_REQUIRE_DOCKER_TESTS=1` (or `CI=true`):

```bash
# CI mode: Docker is mandatory, skip is forbidden
CUBEOPS_REQUIRE_DOCKER_TESTS=1 go test ./... -timeout 600s
```

**Bypassing the test cache**: `go test` caches results when the test source,
the package under test, and the `GO*` environment variables are unchanged.
Business env vars like `CUBEOPS_REQUIRE_DOCKER_TESTS` are **not** part of the
cache key, so setting it alone does not invalidate the cache. To force every
test to re-run (e.g. when verifying a refactor), add `-count=1`:

```bash
# Force re-run, ignoring the cache entirely
go test ./... -timeout 600s -count=1
```

`go clean -testcache` clears the cache globally for the same effect.

### Test categories

| Category | Files | Docker? | What it covers |
|----------|-------|---------|----------------|
| **Unit tests** | `config/config_test.go`, `crypto/aes_gcm_test.go`, `httputil/response_test.go`, `service/auth_test.go` | No | Pure function logic: YAML parsing, AES-GCM encryption, JSON helpers, auth service business logic |
| **HTTP handler tests** | `handler/sdk_test.go`, `handler/cluster_test.go`, `handler/store_test.go`, `auth/handler_test.go` | No | gin routing, middleware, JSON request/response, error code mapping — uses fake CubeMasterClient |
| **Integration tests** | `store/agenthub_test.go`, `handler/agenthub_integration_test.go` | **Yes** | Full HTTP → gin → handler → real MySQL chain — spins up throwaway MySQL 8.0 containers via `dockertest` |

### Running specific test categories

```bash
# Only unit tests (fastest, no Docker)
go test ./internal/config/... ./internal/crypto/... ./internal/httputil/... ./internal/service/...

# Only handler tests (no Docker, uses fake CubeMasterClient)
go test ./internal/handler/... -run 'TestSDK|TestCluster|TestStore|TestConfig' -v

# Only auth handler tests (no Docker, uses fake user store)
go test ./internal/auth/... -v

# Only store integration tests (requires Docker)
go test ./internal/store/... -v -timeout 120s

# Only agenthub handler integration tests (requires Docker)
go test ./internal/handler/... -run TestAgentHub -v -timeout 600s
```

### Integration test details

Integration tests use [`github.com/ory/dockertest/v3`](https://github.com/ory/dockertest)
to spin up throwaway MySQL 8.0 containers. Each test gets its own fresh
database — migrations run automatically, the master encryption key is
bootstrapped, and the default admin account is seeded.

**Prerequisites**:
- Docker daemon must be running and reachable
- The test image `mysql:8.0` will be pulled automatically on first run

**Without Docker**: integration tests are automatically skipped with
`t.Skip()`. Set `CUBEOPS_REQUIRE_DOCKER_TESTS=1` (or `CI=true`) to turn
that into a hard failure instead — useful in CI to catch regressions where
Docker silently went missing. See "Run all tests" above for the exact
command.

**External MySQL**: if you have a MySQL instance you'd like to use instead
of Docker, set `CUBEMASTER_DAO_TEST_MYSQL_DSN`:

```bash
export CUBEMASTER_DAO_TEST_MYSQL_DSN="root:pass@tcp(127.0.0.1:3306)/cube_test"
go test ./internal/store/... -v
```

## Dependencies

- [CubeDB](../CubeDB) — Shared database migration & DAO package
- [CubeMaster](../CubeMaster) — Cluster orchestrator (HTTP API)
- MySQL 8.0 — Shared database
- Docker — For store image metadata (`docker pull` + `docker image inspect` to fetch size/digest of template images)
