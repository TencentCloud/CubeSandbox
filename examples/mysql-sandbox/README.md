# MySQL Client Sandbox

[中文文档](README_zh.md)

A lightweight sandbox environment with MySQL client pre-installed for connecting to external MySQL databases. Ideal for testing database operations, running migrations, and executing SQL queries in an isolated environment.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform fully compatible with the [E2B SDK](https://e2b.dev). This MySQL client sandbox provides:

- Pre-installed MySQL client tools (`mysql`, `mysqldump`)
- Network isolation capabilities (can restrict outbound access)
- Snapshot and restore for stateful database testing
- Hardware-level isolation for running untrusted SQL scripts

```
┌──────────────────────┐         ┌─────── Cube Sandbox ──────────────┐
│                      │         │                                    │
│  Your Script         │  MySQL  │  ┌───────────────────────────┐    │
│  (Python / Shell)   │────────►│  │  mysql client             │    │
│                      │   Wire  │  │  /usr/bin/mysql           │    │
│                      │         │  └───────────────────────────┘    │
└──────────────────────┘         │                                    │
                                 │  ┌───────────────────────────┐    │
                                 │  │  External MySQL Server     │    │
                                 │  │  (any accessible host)   │    │
                                 │  └───────────────────────────┘    │
                                 └────────────────────────────────────┘
```

## 2. Use Cases

- **Database Testing**: Run SQL queries and migrations against test databases
- **Data Migration**: Execute `mysqldump` operations in an isolated environment
- **ORM Validation**: Test database connections for various frameworks
- **Security Testing**: Run untrusted SQL scripts with network isolation
- **CI/CD Integration**: Database operations in ephemeral, isolated environments

## 3. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+

```bash
pip install -r requirements.txt
```

## 4. Quick Start

### Step 1 — Build and Create Template

```bash
# Build the Docker image
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox

# Register as Cube template
cubemastercli tpl create-from-image \
    --image cubesandbox-mysql-sandbox:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

Note the `template_id` printed on success (format: `tpl-xxxxxxxxxxxxxxxx`).

### Step 2 — Configure Environment Variables

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID
```

Or export directly:

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>
```

### Step 3 — Run the Example

```bash
python check_mysql.py
```

Expected output:

```
============================================================
MySQL Client Sandbox - Environment Check
============================================================
Template ID: tpl-xxxxxxxxxxxxxxxx
API URL: http://<node-ip>:3000
============================================================

[1] Creating sandbox...
[2] Sandbox created: sb-xxxxxxxxxxxxxxxx

[3] Checking MySQL client version...
    MySQL version: mysql  Ver 8.0.xx ...

[4] Checking available database tools...
    /usr/bin/mysql
    /usr/bin/mysqldump

[5] Verifying sandbox environment...
    PRETTY_NAME="Debian GNU/Linux 12"
    Kernel: Linux ... x86_64 GNU/Linux

============================================================
Sandbox verification completed!
============================================================
```

## 5. All Examples

| Script | Demonstrates |
|--------|--------------|
| `check_mysql.py` | Basic MySQL client environment check |
| `run_query.py` | Execute SQL queries against a MySQL server |
| `multi_query.py` | Multi-step database operations (DDL + DML) |
| `snapshot_demo.py` | Snapshot, modify, and restore sandbox state |
| `network_isolated.py` | Network isolation with no internet access |

### check_mysql.py — Environment Check

Verifies that the MySQL client and tools are properly installed:

```python
with Sandbox.create(template=template_id) as sandbox:
    # Check MySQL client version
    result = sandbox.commands.run("mysql --version")
    print(result.stdout)
```

### run_query.py — Execute SQL Queries

Execute SQL queries against a MySQL server:

```bash
export DB_HOST=your-mysql-server.com
export DB_USER=testuser
export DB_PASSWORD=testpass
python run_query.py
```

### multi_query.py — Multi-Step Database Operations

Run multiple queries in sequence (CREATE, INSERT, SELECT):

```bash
export DB_HOST=your-mysql-server.com
export DB_USER=testuser
export DB_PASSWORD=testpass
export DB_NAME=smoke
export MYSQL_SANDBOX_ALLOW_DROP=1
python multi_query.py
```

> **Security Note**: The DROP DATABASE operation is protected by two-factor confirmation:
> 1. `MYSQL_SANDBOX_ALLOW_DROP=1` environment variable
> 2. Database name must start with `cube_demo_` prefix

### snapshot_demo.py — Snapshot and Restore

Demonstrates CubeSandbox's snapshot capabilities for database testing:

```python
with Sandbox.create(template=template_id) as sandbox:
    # Create initial snapshot
    snapshot_id = sandbox.create_snapshot()

    # Make changes (e.g., create tables, insert data)
    sandbox.commands.run("mysql -h $DB_HOST -u $DB_USER ...")

    # Restore to initial state
    sandbox.restore_snapshot(snapshot_id)
```

### network_isolated.py — Network Isolation

Create a sandbox with no outbound internet access for secure testing:

```python
sandbox = Sandbox.create(
    template=template_id,
    allow_internet_access=False  # No outbound network
)
```

## 6. Connecting to a MySQL Server

### Within the Sandbox

```bash
# Connect to MySQL server
mysql -h <host> -P <port> -u <user> -p<password>

# Run SQL file
mysql -h <host> -u <user> < database.sql

# Export database
mysqldump -h <host> -u <user> --all-databases > backup.sql
```

### Environment Variables

Set these when creating the sandbox:

```python
sandbox = Sandbox.create(
    template=template_id,
    envs={
        "DB_HOST": "your-mysql-host.com",
        "DB_PORT": "3306",
        "DB_USER": "testuser",
        "DB_PASSWORD": "testpass",
        "DB_NAME": "testdb"
    }
)
```

## 7. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|--------------|-----|
| `mysql: command not found` | Template not built correctly | Rebuild Docker image |
| `Connection refused` | MySQL server not reachable | Check DB_HOST and network policy |
| `Access denied` | Wrong credentials | Verify DB_USER and DB_PASSWORD |
| `Can't connect to MySQL server` | Firewall or network policy | Check allow_out rules |

## 8. Directory Structure

```
mysql-sandbox/
├── Dockerfile              # Docker image definition
├── README.md              # English documentation (this file)
├── README_zh.md           # Chinese documentation
├── check_mysql.py         # Environment check script
├── run_query.py           # Execute SQL queries
├── multi_query.py         # Multi-step database operations
├── snapshot_demo.py       # Snapshot and restore demo
├── network_isolated.py    # Network isolation demo
├── env_utils.py           # Environment variable utilities
├── requirements.txt       # Python dependencies
├── ruff.toml             # Code linting configuration
├── .env.example           # Environment variable template
├── VERIFICATION.md        # Verification guide for contributors
└── screenshots/          # Verification screenshots
    ├── 01_check_mysql.png
    ├── 02_network_isolated.png
    ├── 03_snapshot_demo.png
    └── 04_tpl_list.png
```
