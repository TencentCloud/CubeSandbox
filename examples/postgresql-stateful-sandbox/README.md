# Stateful PostgreSQL Sandbox

[中文文档](README_zh.md)

Run PostgreSQL 16.14 as a self-contained, stateful service inside a
CubeSandbox MicroVM. The database accepts local Unix-socket connections only;
host-side Python drivers execute SQL through Cube's authenticated `envd`
command channel.

This example demonstrates three complete workflows:

- `smoke.py`: create a database, execute SQL, and verify inbound and outbound
  network restrictions.
- `snapshot_restore.py`: checkpoint a running database, make destructive data
  and schema changes, then restore the snapshot into a new sandbox.
- `migration_branches.py`: create two independent migration branches from one
  database snapshot and prove that their files, schemas, and migration ledgers
  remain isolated.

The image is intended for development, testing, migration rehearsal, and agent
workloads. It is not a production PostgreSQL deployment or backup system.

## Architecture and security model

```text
host-side Python script
    |-- control plane: create, snapshot, and delete
    |   `-- CubeAPI -> CubeMaster
    `-- authenticated command and file traffic
        `-- CubeProxy -> envd :49983
                            `-- command executed as OS user postgres
                                `-- PostgreSQL Unix socket
                                    /var/run/postgresql
```

The native SDK obtains sandbox lifecycle state from CubeAPI and uses the
per-sandbox traffic token returned at creation time for requests routed through
CubeProxy to envd. PostgreSQL itself remains reachable only inside the MicroVM.

The container process tree is:

```text
tini
`-- cube-entrypoint.sh
    |-- postgres-ready-envd.sh (background child)
    |   `-- wait for pg_isready, then start envd :49983
    `-- start-postgres.sh (background child)
        `-- postgres (foreground within the helper)
```

`cube-entrypoint.sh` waits for and reaps both children. On TERM or INT it waits
for PostgreSQL to finish its fast shutdown before stopping envd and exiting.
If envd exits unexpectedly, the entrypoint stops PostgreSQL cleanly and exits
non-zero instead of leaving a database process running without its command
channel.
The readiness endpoint is deliberately delayed until `pg_isready` succeeds.
Therefore, an HTTP 204 from `:49983/health` means both the Cube command channel
and PostgreSQL are ready. `POSTGRES_READY_TIMEOUT` defaults to 60 seconds; a
value of `0` performs one immediate readiness check and fails if it is not ready.

The database boundary is intentionally narrow:

- PostgreSQL listens on no TCP address; port 5432 is not registered in the
  Cube template.
- Local connections use `peer` authentication and host connections are
  rejected by `pg_hba.conf`.
- No PostgreSQL password is created or stored.
- Every sandbox and snapshot branch disables public internet egress.
- `network.allow_public_traffic=false` protects inbound `envd` traffic with a
  per-sandbox token that the native SDK supplies automatically.

The conventional UID 1000 `user` account has no `sudo` access. Database
commands are explicitly executed as the `postgres` operating-system user
through envd. The sandbox's root user can still become `postgres`; the MicroVM,
not PostgreSQL roles, is the trust boundary, so do not share one instance
between mutually untrusted users.

## Directory layout

```text
postgresql-stateful-sandbox/
|-- Dockerfile                 # PostgreSQL 16.14 + Cube envd template image
|-- .dockerignore
|-- cube-entrypoint.sh         # Wait for PostgreSQL to shut down cleanly
|-- start-postgres.sh          # Unix-socket-only PostgreSQL foreground process
|-- postgres-ready-envd.sh     # Start envd only after the database is ready
|-- .env.example               # Host SDK configuration
|-- requirements.txt           # cubesandbox and python-dotenv
|-- env_utils.py               # .env loading and validation
|-- postgres_utils.py          # Shared SQL, snapshot, and cleanup helpers
|-- smoke.py                   # Minimal SQL and network-isolation check
|-- snapshot_restore.py        # Data + schema restore demonstration
|-- migration_branches.py      # Two isolated branches from one snapshot
|-- sql/
|   |-- base_schema.sql        # Accounts table, seed data, migration ledger
|   |-- add_email.sql          # Branch A migration
|   `-- add_last_login.sql     # Branch B migration
|-- README.md
`-- README_zh.md
```

## Prerequisites

- A running CubeSandbox deployment with CubeAPI reachable from the host.
- `cubemastercli` installed, on `PATH`, and connected to CubeMaster.
- Docker with `linux/amd64` build support.
- An OCI registry that every target Cube node can pull from.
- Python 3.9 or later for the host-side scripts.
- Cluster capacity for 2 vCPU and 2 GiB RAM per sandbox. The migration branch
  demo runs two sandboxes concurrently, so reserve at least 4 vCPU and 4 GiB
  RAM, plus platform overhead.

## 1. Build and verify the image locally

Build the one supported image:

```bash
export LOCAL_IMAGE=cubesandbox-postgresql-stateful:16.14

docker build --platform linux/amd64 \
  --tag "$LOCAL_IMAGE" \
  examples/postgresql-stateful-sandbox
```

Start it and wait for the combined envd/PostgreSQL health check:

```bash
docker run --detach \
  --name cubesandbox-postgresql-stateful \
  --publish 49983:49983 \
  "$LOCAL_IMAGE"

status=starting
attempt=0
while [ "$attempt" -lt 60 ]; do
  status=$(docker inspect --format '{{.State.Health.Status}}' \
    cubesandbox-postgresql-stateful)
  [ "$status" = healthy ] && break
  if [ "$status" = unhealthy ]; then
    docker logs cubesandbox-postgresql-stateful
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 2
done

if [ "$status" != healthy ]; then
  docker logs cubesandbox-postgresql-stateful
  echo 'ERROR: container did not become healthy within 120 seconds' >&2
  exit 1
fi

curl --fail --silent --show-error \
  --output /dev/null \
  --write-out 'envd health: %{http_code}\n' \
  http://127.0.0.1:49983/health
```

The final command must print `envd health: 204`. Verify the database version,
data directory, and Unix socket:

```bash
docker exec --user postgres cubesandbox-postgresql-stateful \
  psql -X -h /var/run/postgresql -U postgres -d postgres \
  -Atqc 'SHOW server_version; SHOW data_directory; SELECT 1;'
```

Expected values include:

```text
16.14 ...
/var/lib/postgresql/cube-data
1
```

TCP must remain unavailable:

```bash
if docker exec --user postgres cubesandbox-postgresql-stateful \
  pg_isready -h 127.0.0.1 -p 5432 -U postgres -d postgres; then
  echo 'ERROR: PostgreSQL unexpectedly accepted TCP connections' >&2
  exit 1
else
  echo 'OK: PostgreSQL TCP port is closed'
fi
```

Finally, confirm a graceful stop and remove the local container:

```bash
docker stop --time 15 cubesandbox-postgresql-stateful
docker rm cubesandbox-postgresql-stateful
```

## 2. Push and register the Cube template

Tag the image for a registry visible to the Cube nodes:

```bash
export REGISTRY_IMAGE=registry.example.com/cubesandbox-postgresql-stateful:16.14

docker tag "$LOCAL_IMAGE" "$REGISTRY_IMAGE"
docker push "$REGISTRY_IMAGE"
```

Replace `registry.example.com` with your registry, then submit the template
build. `--detach` makes the job ID available for the explicit watch step:

```bash
cubemastercli tpl create-from-image \
  --image "$REGISTRY_IMAGE" \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --cpu 2000 \
  --memory 2048 \
  --allow-internet-access=false \
  --with-cube-ca=false \
  --detach
```

`--with-cube-ca=false` avoids baking a deployment-specific CubeEgress MITM CA
into this offline template. Enable it only if the template is changed to use
intercepted HTTPS egress and must trust CubeEgress's generated certificates.

Copy the printed `job_id` and `template_id`, then wait for all target-node
replicas to become ready:

```bash
cubemastercli tpl watch --job-id <job_id>
cubemastercli tpl info --template-id <template_id> --json --include-request
cubemastercli tpl render --template-id <template_id> --json
```

Do not continue until the job is `READY` and `distribution` reports every
target node ready. Inspect the stored and rendered requests and confirm:

- only container port 49983 is exposed;
- the probe is `GET /health` on 49983;
- CPU is 2000 millicores and memory is 2048 MB;
- the writable layer is 4 GiB;
- internet access is disabled.

The template-level egress restriction is defense in depth. Each Python driver
also passes `allow_internet_access=False` explicitly, including when a snapshot
is used as a template.

## 3. Configure the host-side drivers

```bash
cd examples/postgresql-stateful-sandbox
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Edit `.env`:

```dotenv
CUBE_API_URL=http://127.0.0.1:3000
CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxx

# Set these when the host cannot resolve the Cube sandbox wildcard domain.
# CUBE_PROXY_NODE_IP=<cube-proxy-node-ip>
# CUBE_PROXY_PORT_HTTP=80
# CUBE_SANDBOX_DOMAIN=cube.app
```

| Variable | Required | Purpose |
|---|:---:|---|
| `CUBE_API_URL` | Yes | CubeAPI control-plane endpoint |
| `CUBE_TEMPLATE_ID` | Yes | Template ID produced in step 2 |
| `CUBE_PROXY_NODE_IP` | No | Dial CubeProxy directly instead of relying on wildcard DNS |
| `CUBE_PROXY_PORT_HTTP` | No | CubeProxy HTTP port; defaults to `80` |
| `CUBE_SANDBOX_DOMAIN` | No | Sandbox routing domain; defaults to `cube.app` |

These examples use the native `cubesandbox` SDK, not the E2B SDK, so they do
not require `E2B_API_URL` or `E2B_API_KEY`.

## 4. Run the three workflows

Run the scripts from this directory. They raise an exception on a failed
assertion and print a single `OK:` line only after all checks pass.

### Minimal SQL and network isolation

```bash
python smoke.py
```

The script creates the baseline schema, verifies PostgreSQL 16.14, and checks two
seeded accounts with a total balance of 300. It confirms that sandbox creation
returns a traffic token, that token-authenticated envd health returns 204, and
that the same request without a token does not receive envd's 204 success
response. The exact anonymous error status is a CubeProxy contract rather than
a PostgreSQL template contract. The script then proves that an outbound request
to `example.com` fails, verifies local SQL still works, and confirms that TCP
5432 is closed.

Expected final line:

```text
OK: PostgreSQL template is ready and network isolation is enforced
```

### Snapshot restore into a new sandbox

```bash
python snapshot_restore.py
```

The script loads the baseline, completes all transactions, runs `CHECKPOINT`,
and creates a snapshot. It then sets both balances to zero and adds a
`poisoned` column, verifies those changes, and destroys the source sandbox.
It creates a new sandbox from the snapshot and proves that:

- the restored sandbox has a different ID from the source;
- balances returned to 100 and 200;
- the `poisoned` column disappeared;
- the explicitly created snapshot was deleted during cleanup.

Expected final line:

```text
OK: snapshot restored PostgreSQL data and schema in a new sandbox
```

### Isolated migration branches

```bash
python migration_branches.py
```

The script checkpoints a source database, creates one snapshot, and destroys
the source sandbox. It then creates two sandboxes concurrently from that same
snapshot with the full inbound/outbound security settings applied to each:

- branch A applies `add_email.sql`;
- branch B applies `add_last_login.sql`.

Both branches upload their migration as `/tmp/migration.sql`. The assertions
prove that the file content, schema columns, and `schema_migrations` rows are
isolated. Cleanup always destroys child sandboxes before deleting the shared
snapshot, including partial-create failures.

Expected final line:

```text
OK: isolated PostgreSQL migration branches passed
```

The script intentionally does not call `Sandbox.clone()`: the current helper
creates children with default create settings. Explicit
`Sandbox.create(template=snapshot_id, ...)` calls ensure the timeout and both
network restrictions are applied to every branch.

## Snapshot consistency

Cube snapshots capture a paused MicroVM's memory and filesystem. This example
keeps `PGDATA` and `pg_wal` together at
`/var/lib/postgresql/cube-data`, outside the volume declared by the upstream
image, so the complete database cluster is included.

Before taking a reusable database snapshot, the scripts:

1. wait for application transactions to commit;
2. run PostgreSQL `CHECKPOINT`;
3. call `create_snapshot()` only after the checkpoint succeeds.

This reduces WAL recovery work after restoring or branch creation. It does not
turn a Cube snapshot into a production backup or point-in-time recovery
archive. Do not move `pg_wal` outside `PGDATA`, create external tablespaces, or
copy these snapshots between PostgreSQL major versions.

Snapshots outlive their source sandboxes and may contain sensitive data. A
`with Sandbox.create(...)` block destroys the sandbox only; it does not delete
snapshots. All scripts therefore call `Sandbox.delete_snapshot()` explicitly.

## Resource guidance

| Resource | Setting | Notes |
|---|---:|---|
| CPU | 2000 millicores | Per sandbox |
| Memory | 2048 MB | Per sandbox; increase for larger datasets or queries |
| Writable layer | 4 GiB | Contains PostgreSQL data and WAL |
| Concurrent branch capacity | 4 vCPU / 4 GiB minimum | Two 2-vCPU/2-GiB branches, excluding platform overhead |

Monitor the writable layer when inserting larger datasets. PostgreSQL may need
temporary disk space in addition to table and WAL storage, and each retained
snapshot consumes persistent image storage.

## Known limitations

- The image contains exactly PostgreSQL 16.14 and targets `linux/amd64`.
- It is a single-node, ephemeral development database: no HA, replication,
  PITR, external tablespaces, TLS listener, or raw PostgreSQL ingress.
- Destroying a sandbox without first taking a snapshot permanently discards
  its state.
- Snapshot compatibility is limited to this image and PostgreSQL major
  version.
- Image construction requires registry/package access; sandbox runtime is
  deliberately offline.
- Cube SDK control-plane operations such as create and snapshot do
  not currently expose a separate per-call timeout. Command execution uses its
  own finite timeout.
- Raw TCP clients cannot connect to port 5432 by design. Use the SDK command
  channel and local Unix socket.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Docker build stalls while pulling Docker Hub/GHCR layers | The Docker daemon cannot use the shell's proxy, or host DNS/direct registry routing is broken | Configure a Docker daemon proxy or registry mirror; alternatively pre-pull and locally tag the two base images. If `apt` also needs the host proxy, pass lowercase `http_proxy`/`https_proxy` build args and use an appropriate build network mode |
| Docker health remains `starting` or turns `unhealthy` | PostgreSQL did not initialize, socket permissions are wrong, or envd did not start | Run `docker logs cubesandbox-postgresql-stateful`; confirm `pg_isready` succeeds as `postgres` |
| Template build probe times out | Port/path mismatch or PostgreSQL did not become ready within the probe window | Expose/probe 49983 at `/health`; inspect the image job and Cubelet logs |
| Template is `READY` but distribution is not complete | One or more target nodes have not received the artifact | Wait and rerun `tpl info`; inspect the affected Cubelet's image/storage logs |
| A manual envd request is rejected | The request omitted the per-sandbox traffic token | Use the same returned `Sandbox` object; for manual HTTP requests supply its traffic token header |
| SDK cannot resolve `*.cube.app` | Host DNS cannot resolve the sandbox wildcard domain | Set `CUBE_PROXY_NODE_IP` and, if needed, `CUBE_PROXY_PORT_HTTP` |
| Local CubeAPI calls return HTTP 502 while `HTTP_PROXY`/`HTTPS_PROXY` is set | Loopback control-plane traffic was sent through the host proxy | Add `127.0.0.1`, `localhost`, and the CubeProxy node IP to both `NO_PROXY` and `no_proxy` |
| `psql: Peer authentication failed` | Command ran as the wrong OS user | Execute through the provided helpers or pass `user="postgres"` |
| `pg_isready -h 127.0.0.1` fails | None; this is the expected secure configuration | Use `-h /var/run/postgresql` |
| `No space left on device` | The 4-GiB writable layer is full | Remove test data/snapshots or rebuild the template with a larger writable layer |
| Script exits before its final `OK:` line | An assertion or platform call failed | Read the printed command stdout/stderr, then list sandboxes and snapshots to confirm cleanup |

## Acceptance checklist

Run these checks in the environment where the contribution will be reviewed;
this README does not claim that a particular external cluster has already
passed them.

```bash
python3 -m compileall examples/postgresql-stateful-sandbox
ruff check examples/postgresql-stateful-sandbox
shellcheck examples/postgresql-stateful-sandbox/*.sh
git diff --check

npm --prefix docs ci
npm --prefix docs run docs:build
```

In addition, complete the local image checks in step 1, reach template
`READY` with full distribution, run all three Python scripts, and compare the
sandbox/snapshot lists before and after the run to ensure no resources leaked.

## References

- [Create a template from an OCI image](../../docs/guide/tutorials/template-from-image.md)
- [Connect to an existing Cube cluster](../../docs/guide/connect-existing-cluster.md)
- [Snapshot, rollback, and clone](../../docs/guide/snapshot-rollback-clone.md)
- [Network policy](../../docs/guide/network-policy.md)
- [Restrict public access](../../docs/guide/restrict-public-access.md)
- [PostgreSQL file-system backup consistency](https://www.postgresql.org/docs/16/backup-file.html)
- [PostgreSQL `CHECKPOINT`](https://www.postgresql.org/docs/16/sql-checkpoint.html)
