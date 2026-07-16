# MySQL Sandbox Example

[中文](README_zh.md)

A lightweight MySQL client sandbox for Cube Sandbox, providing isolated SQL execution with network policy controls and snapshot capabilities.

## Overview

This example demonstrates how to build a MySQL client sandbox that can:
- Connect to MySQL databases from an isolated KVM MicroVM
- Enforce network isolation policies (allowlist/denylist)
- Create snapshots for state preservation and rollback

### Key Features

| Feature | Description |
|---------|-------------|
| **Isolated Execution** | Each sandbox runs in an independent KVM MicroVM with full hardware-level isolation |
| **Network Control** | Supports full isolation, CIDR allowlists, denylists, and granular network policies |
| **Snapshot & Rollback** | Create snapshots to save state, restore to any previous state |
| **Flexible Resources** | Customizable CPU, memory, and storage sizes |

## Use Cases

### 1. Database Operations

Execute SQL queries without installing MySQL client locally.

**Advantages**:
- No need to configure local database environment
- Clean, pristine environment for every use
- No risk of polluting local development environment

### 2. AI Agent Integration

Provide SQL execution capabilities to AI agents with security controls.

**Typical Applications**:
- Data Analysis Agent: Allow access to analytics servers, block external networks
- Code Review Agent: Only access internal code repository databases
- Report Generation Agent: Read-only data access, no modifications

### 3. Database Migration Testing

Safely test database migrations in an isolated environment with rollback capability.

**Workflow**:
1. Create baseline snapshot
2. Execute migration scripts
3. Verify data integrity
4. Rollback on failure

### 4. Data Analysis

Query databases from sandboxed environments with reduced data leakage risk.

**Security Features**:
- Data completely cleared after sandbox destruction
- Restrict access to specific databases only
- Audit logging support

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Cube Sandbox                              │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                   KVM MicroVM                             │   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐ │   │
│  │  │  envd       │  │  MySQL      │  │  Snapshot       │ │   │
│  │  │  (port      │  │  Client     │  │  (memory +     │ │   │
│  │  │  49983)     │  │             │  │   rootfs)      │ │   │
│  │  └─────────────┘  └─────────────┘  └─────────────────┘ │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
                      ┌───────────────┐
                      │   MySQL       │
                      │   Database    │
                      └───────────────┘
```

### Component Description

| Component | Description |
|-----------|-------------|
| **KVM MicroVM** | Lightweight VM based on KVM, providing hardware-level isolation |
| **envd** | Environment daemon managing sandbox lifecycle and code execution |
| **MySQL Client** | Pre-installed MySQL command-line client |
| **Snapshot System** | Supports both memory and filesystem snapshots |

## Prerequisites

### Required Conditions

- Deployed Cube Sandbox environment (see [Quick Start](../../docs/guide/quickstart.md))
- Python 3.8+
- Docker (for building custom images)

### Environment Verification

```bash
# Verify Python version
python3 --version
# Should output Python 3.8+

# Verify Docker
docker --version
# Should output Docker version

# Verify Cube environment
cubemastercli --version
# Should output cubemastercli version
```

## Before You Run

> **IMPORTANT: Security warnings — read before using `multi_query.py`!**

This sandbox executes SQL against your MySQL server. Please understand these safeguards:

### Credential Security

The helper `build_mysql_cmd()` in `env_utils.py` intentionally omits `-p<password>`.
The `mysql` client reads `MYSQL_PWD` from the sandbox environment, so credentials
never appear in `/proc/<pid>/cmdline` or `ps aux` inside the sandbox.

**Do not** inject `DB_PASSWORD` into a shell command line. Pass it via
`Sandbox.create(envs=...)` as `MYSQL_PWD` instead.

### Destructive Cleanup in `multi_query.py`

The cleanup step at the end of `multi_query.py` issues `DROP DATABASE`. To prevent
accidental data loss, the DROP is gated by **two** independent signals:

1. The environment variable `MYSQL_SANDBOX_ALLOW_DROP` must be set to a
   truthy value (`1` / `true` / `yes` / `on`).
2. The effective database name must start with the prefix `cube_demo_`
   (the script prepends this prefix automatically, so `DB_NAME=smoke`
   becomes `cube_demo_smoke`).

If either signal is missing the DROP is skipped and the demo database is
left intact.

**Before running against a real server, always set `DB_NAME` to a throwaway value
(e.g. `cube_demo_$(date +%s)`).**

## Quick Start

### Step 1: Build the Docker Image

```bash
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox
```

**Expected Output**:
```
[+] Building 10.5s (8/8) FINISHED
...
 => naming to docker.io/library/cubesandbox-mysql-sandbox:latest
```

### Step 2: Push Image to Registry (Optional)

For multi-node environments:

```bash
# Login to registry
docker login <your-registry>

# Push image
docker tag cubesandbox-mysql-sandbox:latest <your-registry>/cubesandbox-mysql-sandbox:latest
docker push <your-registry>/cubesandbox-mysql-sandbox:latest
```

### Step 3: Register as Cube Template

```bash
cubemastercli tpl create-from-image \
    --image cubesandbox-mysql-sandbox:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

**Parameter Description**:

| Parameter | Description | Recommended |
|-----------|-------------|-------------|
| `--image` | Docker image address | Your image address |
| `--writable-layer-size` | Writable layer size | 1G ~ 5G |
| `--expose-port` | Exposed port | 49983 (envd) |
| `--probe` | Health check port | 49983 |
| `--probe-path` | Health check path | /health |

**Expected Output**:
```
Template created successfully!
Template ID: tpl-xxxxxxxxxxxxxxxx
Status: PENDING
...
Status: READY
```

> **Note**: First-time creation requires pulling the base image and building snapshots, which may take 1-3 minutes. The template is ready for use when Status becomes READY.

Record the output `template_id` for later use.

### Step 4: Configure Environment Variables

```bash
cd /root/CubeSandbox/examples/mysql-sandbox
cp .env.example .env
```

Edit the `.env` file:

```bash
# Required: Cube API server address
E2B_API_URL="http://<node-ip>:3000"

# Required: any non-empty value satisfies SDK validation
E2B_API_KEY="e2b_000000"

# Required: template ID (from Step 3)
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxx"

# Optional: only needed when using a custom CA certificate (e.g., mkcert for local HTTPS).
# On most systems the default CA bundle works without setting this.
# SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"

# Optional: MySQL database connection configuration
DB_HOST="localhost"
DB_USER="root"
DB_PASSWORD=""
DB_NAME="test_db"
```

Or export environment variables directly:

```bash
E2B_API_KEY="e2b_000000"
E2B_API_URL="http://127.0.0.1:3000"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxx"
SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"
```

### Step 5: Install Dependencies

```bash
pip3 install -r requirements.txt
```

**Dependencies**:

| Dependency | Version | Description |
|------------|---------|-------------|
| e2b-code-interpreter | >= 2.4.1 | Cube Sandbox Python SDK |
| python-dotenv | Latest | Environment variable loading |

See the [Example Scripts](#example-scripts) section below for details.

## Example Scripts

Each script runs independently. For the full implementation, see the script files directly.

| Script | Purpose | Prerequisite |
|--------|---------|--------------|
| `check_mysql.py` | Verify MySQL client is available in sandbox | Template only |
| `run_query.py` | Connect to MySQL server and run a query | Reachable MySQL server |
| `multi_query.py` | Execute a batch of SQL statements | Reachable MySQL server |
| `network_isolated.py` | Demonstrate three network isolation policies | Template only |
| `snapshot_demo.py` | Demonstrate snapshot creation and rollback | Template only |

Run the examples:

```bash
cd /root/CubeSandbox/examples/mysql-sandbox
source .env

python3 check_mysql.py        # verify MySQL client
python3 network_isolated.py   # network isolation (no MySQL server needed)
python3 snapshot_demo.py      # snapshot and rollback (no MySQL server needed)
python3 run_query.py          # execute SQL (DB_HOST must be reachable)
python3 multi_query.py        # batch SQL (DB_HOST must be reachable)
```

Expected output sample (`check_mysql.py`):

```
============================================================
MySQL Client Sandbox - Verification Test
============================================================
Template ID: tpl-xxxxxxxxxxxxxxxx
============================================================

[1] Creating sandbox...
[2] Sandbox created successfully!
[3] Checking MySQL client...
    MySQL version: mysql  Ver 15.1 Distrib 10.11.18-MariaDB, ...
[4] Checking available database tools...
    /usr/bin/mysql
    /usr/bin/mysqldump
[5] System information...
    Linux tpl-a067 6.6.69-opencloudos9.cubesandbox.pvm.guest-gb85200d80fa2 ...
[6] Verifying sandbox environment...
    PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"

Sandbox verification completed!
```

## Known Limitations

- The sandbox only ships the MySQL **client**; it needs an external MySQL server. KVM sandboxes are network-isolated from the host, so services on `localhost` / `127.0.0.1` are unreachable (`cube-egress` only proxies `tcp dport 80/443` by default).
- Default `--writable-layer-size 1G` caps the snapshot size; writing large temp files slows down snapshot creation.
- The cluster enforces limits on concurrent sandboxes and snapshot count.

## Troubleshooting

| Problem | Possible Cause | Solution |
|---------|----------------|----------|
| `Template not found` | Wrong template ID or not ready | Run `cubemastercli tpl list` |
| `Connection refused` | MySQL not running or wrong port | Check `DB_HOST` / port; ensure sandbox can route to target IP |
| `Can't connect to server` | Network isolation blocking connection | Adjust `allow_out`/`deny_out` CIDRs |
| `SSL certificate error` | HTTPS without CA bundle configured | Set the `SSL_CERT_FILE` env var |
| `create-from-image` stuck at UNPACKING 20% | Cross-border egress too narrow, registry pull stalls | Switch to an in-region registry, or use `cubemastercli tpl commit` |

Debug example:

```python
with Sandbox.create(template=template_id) as sandbox:
    info = sandbox.get_info()
    print(f"Sandbox ID: {info.sandbox_id}, Status: {info.status}")
    print(sandbox.commands.run("mysql --version").stdout)
```

Template build log:

```bash
cubemastercli tpl info --template-id <template-id>
```

## File Structure

```
mysql-sandbox/
├── Dockerfile                 # Docker image definition
├── README.md                  # English documentation (this file)
├── README_zh.md              # Chinese documentation
├── requirements.txt          # Python dependencies
├── .env.example              # Environment variables template
│
├── env_utils.py              # Shared environment utility functions
│
├── check_mysql.py            # Verify MySQL client installation
├── run_query.py              # Execute SQL query example
├── network_isolated.py       # Network isolation demo
├── snapshot_demo.py           # Snapshot and rollback demo
└── multi_query.py            # Execute multiple queries
```

## Related Examples

| Example | Path | Description |
|---------|------|-------------|
| Basic Sandbox | [code-sandbox-quickstart](../code-sandbox-quickstart/) | Sandbox basics tutorial |
| Network Policy | [network-policy](../network-policy/) | More network configuration examples |
| Snapshot Management | [snapshot-rollback-clone](../snapshot-rollback-clone/) | Full snapshot features |
| AI Agent | [openai-agents-example](../openai-agents-example/) | OpenAI Agents integration |
| Browser Sandbox | [browser-sandbox](../browser-sandbox/) | Playwright browser automation |

For full `Sandbox.create()` / `create_snapshot()` / `list_snapshots()` API usage, see the [e2b-code-interpreter SDK documentation](https://github.com/cube-sandbox/e2b-code-interpreter).

## Contributing

Contributions are welcome! Please follow these steps:

1. Fork this repository
2. Create a feature branch: `git checkout -b feature/my-template`
3. Commit changes: `git commit -m 'Add MySQL sandbox template'`
4. Push branch: `git push origin feature/my-template`
5. Open a Pull Request

Please ensure:
- Code follows project coding standards
- Documentation is clear and complete
- Necessary tests are added

## License

This project follows the license agreement in the project root directory.
