#!/usr/bin/env python3
"""
multi_query.py - Execute multiple SQL queries in sequence.

This script demonstrates:
1. Executing multiple queries in a sandbox
2. Handling query results
3. Batch database operations

This is useful for:
- Database migrations
- Batch data processing
- Automated testing

Destructive operations — read before running
--------------------------------------------

This script **drops the database named in ``DB_NAME``** at the end of the
run as cleanup. A misconfigured ``DB_NAME`` (e.g. ``production``) can
therefore destroy unrelated data on a shared MySQL server.

The DROP is gated by *two* opt-in signals; if either is missing the
script skips the DROP and only leaves behind the test artifacts in
``DB_NAME``:

1. The ``MYSQL_SANDBOX_ALLOW_DROP`` environment variable must be set
   to a truthy value (``1`` / ``true`` / ``yes``).
2. The ``DB_NAME`` value must match the demo prefix
   ``cube_demo_<suffix>`` (the script also creates the database with
   that prefix to keep the demo isolated from real schemas).

For non-interactive / CI use, set both explicitly, e.g.::

    export DB_NAME="cube_demo_smoke_$(date +%s)"
    export MYSQL_SANDBOX_ALLOW_DROP=1
    python3 multi_query.py
"""

import os
import re
import sys

sys.path.insert(0, str(__file__).rsplit("/", 1)[0])

from e2b_code_interpreter import Sandbox

from env_utils import (
    get_api_key,
    get_api_url,
    get_env,
    get_ssl_cert_file,
    get_template_id,
    run_mysql_query,
)

# Regex for valid database name: alphanumeric, underscore, dash, dot (MySQL allows these)
_DB_NAME_PATTERN = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_\-.]*$")


def _ident(name: str) -> str:
    """Return ``name`` wrapped in MySQL backticks to safely quote identifiers.

    Database, table, and column names that collide with MySQL reserved words
    or contain special characters need to be quoted. Backtick quoting is the
    MySQL-native approach; it is NOT the same as SQL-injection mitigation
    (values should still be passed as prepared-statement parameters), but it
    prevents accidentally breaking queries when a name happens to match a
    keyword.
    """
    _b = "`"
    return _b + name.replace(_b, _b + _b) + _b


# Set environment variables for the sandbox
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

# Database configuration
db_host = get_env("DB_HOST", "localhost") or "localhost"
db_user = get_env("DB_USER", "root") or "root"
db_password = get_env("DB_PASSWORD", "") or ""
db_name = get_env("DB_NAME", "") or ""

if db_name and not _DB_NAME_PATTERN.match(db_name):
    raise ValueError(
        f"Invalid DB_NAME value: {db_name!r}. "
        "Only letters, numbers, underscore, dash, and dot are allowed."
    )

# Treat "DROP DATABASE" as a destructive side-effect: it is opt-in.
# See module docstring for the two required signals.
allow_drop_env = get_env("MYSQL_SANDBOX_ALLOW_DROP", "") or ""
allow_drop = allow_drop_env.strip().lower() in ("1", "true", "yes", "on")

print("=" * 60)
print("MySQL Client Sandbox - Multi-Query Execution")
print("=" * 60)
print(f"Template ID: {template_id}")
print(f"DB Host: {db_host}")
print(f"DB User: {db_user}")
print(f"DB Name: {db_name}")
print(f"Cleanup DROP enabled: {allow_drop}")
print("=" * 60)


def main():
    """Execute multiple queries."""
    print("\n[1] Creating sandbox...")

    # Sandbox reads DB_PASSWORD via MYSQL_PWD so the password never appears
    # on the command line (`/proc/<pid>/cmdline`, `ps aux`).
    sandbox_envs = {
        "DB_HOST": db_host,
        "DB_USER": db_user,
        "DB_NAME": db_name,
    }
    if db_password:
        sandbox_envs["MYSQL_PWD"] = db_password
    create_kwargs = {
        "template": template_id,
        "envs": sandbox_envs,
    }
    if ssl_cert:
        create_kwargs["envs"]["SSL_CERT_FILE"] = ssl_cert

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created!")

        # Demo queries (without actual database connection)
        demo_queries = [
            ("Check MySQL client", "mysql --version"),
            ("Check connection", "echo 'Connection test'"),
        ]

        print("\n[3] Running demo queries...")
        for title, query in demo_queries:
            print(f"\n    {title}:")
            result = sandbox.commands.run(query)
            if result.exit_code != 0:
                print(f"    Warning: exit code {result.exit_code}")
            print(f"    {result.stdout.strip() if result.stdout else '(no output)'}")

        # If database is configured, run actual queries. We always work
        # under a `cube_demo_` prefix so the cleanup DROP can never reach
        # an unrelated schema the operator didn't explicitly authorize.
        if db_host and db_name:
            print("\n[4] Running database queries...")
            demo_db = f"cube_demo_{db_name}"

            print(f"    Using isolated demo database: '{demo_db}'")
            print(f"    (Derived from DB_NAME='{db_name}' + 'cube_demo_' prefix)")

            # Identifier quoting: wrap all db/table names in backticks.
            # This is the MySQL-native way to handle names that collide with
            # reserved words or contain special characters.  Values (INSERT data)
            # still use single quotes as usual.
            q_db = _ident(demo_db)
            queries = [
                ("Create test database", f"CREATE DATABASE IF NOT EXISTS {q_db}"),
                ("Use database", f"USE {q_db}"),
                (
                    "Create users table",
                    f"""
CREATE TABLE IF NOT EXISTS {q_db}.`users` (
    `id` INT AUTO_INCREMENT PRIMARY KEY,
    `name` VARCHAR(100) NOT NULL,
    `email` VARCHAR(100),
    `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)""",
                ),
                (
                    "Insert user Alice",
                    f"INSERT INTO {q_db}.`users` (`name`, `email`) VALUES ('Alice', 'alice@example.com')",
                ),
                (
                    "Insert user Bob",
                    f"INSERT INTO {q_db}.`users` (`name`, `email`) VALUES ('Bob', 'bob@example.com')",
                ),
                ("Select all users", f"SELECT * FROM {q_db}.`users`"),
            ]

            for title, query in queries:
                print(f"\n    {title}:")
                stdout = run_mysql_query(sandbox, query, demo_db)
                if stdout:
                    print(f"    {stdout.strip()}")

            print("\n[5] Query results summary:")
            stdout = run_mysql_query(
                sandbox, f"SELECT COUNT(*) AS `count` FROM `{demo_db}`.`users`", ""
            )
            print(f"    Total users: {stdout.strip() if stdout else '(none)'}")

            # Cleanup — DROP is opt-in. Two independent signals must both
            # be present before we issue a destructive statement.
            print("\n[6] Cleanup...")
            if not allow_drop:
                print("    ⊘ Skipping DROP DATABASE (MYSQL_SANDBOX_ALLOW_DROP not set).")
                print(f"    The demo database '{demo_db}' is left intact on the server.")
                print("    To enable auto-cleanup, run:")
                print('        export MYSQL_SANDBOX_ALLOW_DROP=1  # and use a "cube_demo_" DB_NAME')
            else:
                # Even with the env var set, refuse to drop a database whose
                # name does NOT start with `cube_demo_` — this catches the
                # "user fat-fingered DB_NAME=production" scenario.
                if not demo_db.startswith("cube_demo_"):
                    print(
                        f"    ⊘ Refusing to DROP '{demo_db}' — name does not start with 'cube_demo_'."
                    )
                    print("    Aborting cleanup to protect unrelated data.")
                else:
                    # Belt-and-suspenders: show the databases that exist so the
                    # operator can visually confirm the target before DROP runs.
                    listing = run_mysql_query(sandbox, "SHOW DATABASES", "")
                    print(f"    Databases currently visible:\n{listing}")
                    print(f"    >>> About to DROP DATABASE: {demo_db!r} <<<")
                    run_mysql_query(sandbox, f"DROP DATABASE IF EXISTS {_ident(demo_db)}", "")
                    print(f"    ✓ Demo database '{demo_db}' dropped")
        else:
            print("\n[4] Skipping database queries (DB_HOST or DB_NAME not set)")
            print("    Set DB_HOST and DB_NAME environment variables to run queries")

        print("\n" + "=" * 60)
        print("Multi-query execution completed!")
        print("=" * 60)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"\nError: {e}")
        import traceback

        traceback.print_exc()
        sys.exit(1)
