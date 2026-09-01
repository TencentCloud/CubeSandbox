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
   `/api/v1/store`, `/api/v1/config`, `/api/v1/warehouse`) — used by the WebUI for cluster
   management, digital assistant (AgentHub) lifecycle, store operations, and the
   component warehouse.
2. **SDK API** (`/api/v1/sdk/*`) — used by the WebUI for sandbox/template/
   snapshot CRUD. These endpoints call CubeMaster HTTP REST API directly
   (replacing the former CubeAPI reverse proxy).
3. **Node warehouse API** (`/internal/warehouse/*`) — unauthenticated, same
   isolation model as CubeMaster `/internal/meta`. Compute nodes download
   pinned component versions here. Do not publish this prefix on the public
   WebUI nginx.

> **Multi-replica**: CubeOps node state lives in shared MySQL/Redis, so it can be scaled beyond one replica for availability.

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
> `CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK=1`.
>
> **Disabling auto-migration**: in production environments where the
> runtime database account has DML-only grants (no DDL), set
> `CUBE_AUTO_MIGRATION=false` to skip schema migration at startup. The
> schema must then be applied out-of-band by a privileged account. Default
> is enabled (migrate on boot).

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

# View recent logs (file-based, via cubelog)
tail -50 /data/log/CubeOps/cubeops-req.log

# Follow logs in real-time
tail -f /data/log/CubeOps/cubeops-req.log
```

Every HTTP request receives an `X-RequestID` when one is not supplied. The
request trace is attached to the request context, so request-scoped `cubelog`
records include fields such as `RequestId`, `Action`, `Caller`, and
`CalleeAction`. CubeMaster calls propagate `X-Caller: cubeops` and stamp the
inbound `RequestID` into the outbound body and query string — that is how
cross-service correlation works (CubeMaster ignores `X-RequestID`).

Quick health check (no auth required):

```bash
curl -s http://127.0.0.1:3010/health
# → ok
```

## Configuration

One-click and Helm configure CubeOps with environment variables. Nested YAML
keys map to `CUBE_OPS_<SECTION>_<FIELD>` (for example `s3.endpoint` →
`CUBE_OPS_S3_ENDPOINT`). Env wins over a YAML file, which wins over built-in
defaults.

### Environment variables (one-click / Helm)

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_OPS_BIND` | `127.0.0.1:3010` | Listen address. In All-in-One deployments this must be set to `0.0.0.0:3010` so the WebUI nginx container can reach CubeOps via `host.docker.internal:3010`. |
| `CUBE_OPS_LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `CUBE_OPS_LOG_DIR` | `/data/log/CubeOps` | Log file directory. Logs are written to `cubeops-req.log` (business) and `cubeops-stat.log` (trace), rotated by size. |
| `CUBE_OPS_LOG_FILE_NUM` | `10` | Number of rotated log files to retain. |
| `CUBE_OPS_LOG_FILE_SIZE` | `100` | Max size in MB per log file before rotation. |
| `JWT_SECRET` | *(optional)* | JWT signing secret. If unset, a secret is auto-generated on first startup and persisted to the `t_system_setting` table in the database. |
| `JWT_ACCESS_TTL` | `15m` | Access token TTL |
| `JWT_REFRESH_TTL` | `168h` | Refresh token TTL (7 days) |
| `DATABASE_URL` | *(required)* | MySQL connection URL. If unset, built from `CUBE_SANDBOX_MYSQL_{HOST,PORT,USER,PASSWORD,DB}` env vars. |
| `CUBE_MASTER_ADDR` | `http://127.0.0.1:8089` | CubeMaster base URL |
| `CUBE_API_SANDBOX_DOMAIN` | `cube.app` | Sandbox domain (used by SDK handler for sandbox URL construction) |
| `REDIS_URL` | *(optional)* | Redis connection URL. Alternatively use `REDIS_HOST`/`REDIS_PORT`/`REDIS_PASSWORD`. |
| `CUBE_OPS_WAREHOUSE_WORK_DIR` | `/var/tmp/cubeops-warehouse` | Local unpack scratch (`warehouse.work_dir`); also used as `TMPDIR` |
| `CUBE_OPS_WAREHOUSE_UPLOAD_TIMEOUT` | `30m` | HTTP write timeout and S3 Put abort budget (`warehouse.upload_timeout`) |
| `CUBE_OPS_WAREHOUSE_FETCH_TIMEOUT` | `30m` | GitHub/CNB download timeout for one-click imports (`warehouse.fetch_timeout`) |
| `CUBE_OPS_WAREHOUSE_UPLOAD_MAX_BYTES` | `8GiB` | Max one-click upload size (`warehouse.upload_max_bytes`) |
| `CUBE_OPS_S3_ENDPOINT` | empty | Empty (with empty AK/SK) disables warehouse routes (`501 warehouse_disabled`) |
| `CUBE_OPS_S3_NODE_ENDPOINT` | same as endpoint | Host signed into presigned GET URLs |
| `CUBE_OPS_S3_ACCESS_KEY_ID` / `SECRET_ACCESS_KEY` | empty | Object-store credentials (never given to nodes) |
| `CUBE_OPS_S3_BUCKET` | `cube-ops` | CubeOps dedicated bucket, separate from `cube-volumes` / `cube-s3lvol` |
| `CUBE_OPS_WAREHOUSE_PRESIGN_TTL` | `5m` | Presigned GET TTL (clamped 1m–15m) |
| `CUBE_OPS_WAREHOUSE_GITHUB_REPOS` | `TencentCloud/CubeSandbox` | Comma-separated GitHub owner/repo allow-list (`warehouse.github_repos`) |
| `CUBE_OPS_WAREHOUSE_CNB_REPOS` | `CubeSandbox/CubeSandbox` | Comma-separated CNB owner/repo allow-list (`warehouse.cnb_repos`) |
| `CUBE_OPS_WAREHOUSE_GITHUB_TOKEN` | *(optional)* | Token for private GitHub release downloads (`warehouse.github_token`) |
| `CUBE_OPS_WAREHOUSE_CNB_TOKEN` | *(optional)* | Token for private CNB release downloads (`warehouse.cnb_token`) |

Warehouse blobs use the CubeOps S3 bucket (`cube-ops`):

- Minimum IAM: `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`,
  `s3:ListBucket`, `s3:AbortMultipartUpload`, `s3:ListBucketMultipartUploads`
  (plus `s3:GetBucketLocation` if region is unset).
- Nodes never receive credentials; they only GET a presigned URL.
- Anyone who can write the bucket can change the binaries nodes execute —
  treat AK/SK as a trust boundary.

The systemd unit reads environment variables from `.one-click.env` via the
start script at `deploy/one-click/scripts/systemd/cubeops-start.sh`.

### YAML file (optional, manual installs)

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
with inline comments. One-click does not install this file.

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

### Warehouse

The warehouse catalog is a closed set of four components (`cube-shim`,
`cube-image`, `cube-agent`, `cube-kernel-scf`). Artifacts stay keyed by
`(arch, component, version)`. `GET /warehouse/components` always returns
those four names (empty warehouse: `versionCount: 0`). Unknown names on
`GET /warehouse/components/{component}` are `400`. If CubeOps cannot
list nodes, `nodesMissing` is omitted rather than reported as zero.

- `GET /api/v1/warehouse/components` — Catalog summaries (`name`, `versionCount`, `arches`, `sizeBytes`, optional `nodesMissing`)
- `GET /api/v1/warehouse/components/{component}` — Versions grouped with per-arch artifacts and node coverage
- `DELETE /api/v1/warehouse/components/{component}/versions/{version}?arch=` — Delete a warehouse copy (not node-local inventory)
- `POST /api/v1/warehouse/uploads` — Upload a one-click `.tar.gz`
- `POST /api/v1/warehouse/imports` — Start an async import (github / cnb / upload)
- `GET /api/v1/warehouse/imports` — List import jobs (`?limit=&offset=`; response `{jobs, total}`)
- `GET /api/v1/warehouse/imports/{id}` — Import job status
- `POST /api/v1/warehouse/preinstall` — Create per-node pull jobs
- `GET /api/v1/warehouse/preinstall` — List preinstall jobs (`?limit=&offset=&node_id=&status=`; response `{jobs, total}`)
- `GET /internal/warehouse/blob` — Node download of one version tree (no JWT)
- `GET /internal/warehouse/jobs` — Pending preinstall jobs for this node
- `POST /internal/warehouse/jobs/{id}/ack` — Ack running/succeeded/failed
- `PUT /internal/warehouse/inventory` — Replace this node's inventoried versions for one arch (empty `items` clears that node+arch)

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
- `GET /api/v1/store/meta` — Cached image metadata from previous refreshes (no network call)
- `POST /api/v1/store/refresh` — Refresh image digests and sizes via the OCI distribution API (go-containerregistry, no docker required)

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
