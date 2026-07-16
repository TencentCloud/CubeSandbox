#!/usr/bin/env python3
"""
multi_query.py - Execute multiple SQL queries in sequence inside a sandbox.

This script demonstrates:
1. Executing multiple queries in a sandboxed MySQL client session
2. Handling query results and exit codes
3. Batch database operations (CREATE, INSERT, SELECT)

Use cases:
- Database migrations with multiple steps
- Batch data processing and imports
- Automated testing with setup/teardown

Destructive operations — read before running
--------------------------------------------

This script **drops the database named in ``DB_NAME``** at the end as cleanup.
A misconfigured ``DB_NAME`` (e.g. ``production``) can destroy unrelated data
on a shared MySQL server.

The DROP is gated by *two* independent opt-in signals; if either is missing the
script skips the DROP:

1. ``MYSQL_SANDBOX_ALLOW_DROP`` environment variable must be truthy
   (``1`` / ``true`` / ``yes`` / ``on``).
2. ``DB_NAME`` must start with the ``cube_demo_`` prefix. This prefix is also
   prepended automatically, so ``DB_NAME=smoke`` becomes ``cube_demo_smoke``.
   This prevents the DROP from reaching any schema the operator didn't
   explicitly authorize.

Example safe usage::

    export DB_NAME="cube_demo_smoke_$(date +%s)"
    export MYSQL_SANDBOX_ALLOW_DROP=1
    python3 multi_query.py

For interactive use, the script also prompts for confirmation before dropping.
"""

import os
import re
import sys

# Ensure env_utils can be imported from any working directory.
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

# MySQL has a 64-character limit on identifier lengths (database, table, column names).
_MYSQL_MAX_IDENT = 64
# All demo databases are prefixed with this string so the cleanup DROP can never
# reach a schema the operator didn't explicitly authorize.
_PREFIX = "cube_demo_"
_PREFIX_LEN = len(_PREFIX)  # 10 characters
# Maximum length for the user-provided DB_NAME suffix to stay within the 64-char limit.
_MAX_SUFFIX_LEN = _MYSQL_MAX_IDENT - _PREFIX_LEN  # 54 characters

# Valid database name pattern: MySQL allows letters, digits, underscore, dash, dot.
# Must start with a letter or underscore (MySQL rule).
_DB_NAME_PATTERN = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_\-.]*$")


def _ident(name: str) -> str:
    """Return ``name`` wrapped in MySQL backticks for safe identifier quoting.

    Database, table, and column names that collide with MySQL reserved words
    (e.g., ``SELECT``, ``TABLE``, ``INDEX``) or contain special characters need
    to be quoted. Backtick quoting is the MySQL-native approach.

    Note: Backtick quoting is NOT the same as SQL-injection mitigation.
    User-provided **values** (e.g., in WHERE clauses) should still be passed
    as parameterized query parameters, not via string concatenation or backtick
    quoting.

    Args:
        name: Identifier to quote (database name, table name, column name, etc.).

    Returns:
        The identifier wrapped in backticks, with any embedded backticks doubled.
    """
    _b = "`"
    return _b + name.replace(_b, _b + _b) + _b


# Load required configuration from environment.
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

# Database configuration (optional — script works without DB connection).
db_host = get_env("DB_HOST", "localhost") or "localhost"
db_user = get_env("DB_USER", "root") or "root"
db_password = get_env("DB_PASSWORD", "") or ""
db_name = get_env("DB_NAME", "") or ""

# Validate DB_NAME format before doing anything else.
if db_name and not _DB_NAME_PATTERN.match(db_name):
    raise ValueError(
        f"Invalid DB_NAME value: {db_name!r}. "
        "Only letters, numbers, underscore, dash, and dot are allowed."
    )

# Check MySQL identifier length: DB_NAME gets prefixed with "cube_demo_",
# and the full name must fit within MySQL's 64-character limit.
if db_name and len(db_name) > _MAX_SUFFIX_LEN:
    raise ValueError(
        f"DB_NAME value too long: {db_name!r} ({len(db_name)} chars). "
        f"The 'cube_demo_' prefix plus DB_NAME must not exceed {_MYSQL_MAX_IDENT} characters. "
        f"Please use a DB_NAME with at most {_MAX_SUFFIX_LEN} characters."
    )

# Treat "DROP DATABASE" as a destructive side-effect: both signals are required.
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
    """Execute multiple queries in a sandboxed MySQL client session."""
    print("\n[1] Creating sandbox...")

    # Inject DB credentials as environment variables so they never appear on
    # the command line. MYSQL_PWD is the standard way to pass the password
    # without exposing it to /proc/<pid>/cmdline or `ps aux`.
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

        # Basic sanity checks that work without a database connection.
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

        # If both DB_HOST and DB_NAME are set, run actual database queries.
        # The demo always operates under the "cube_demo_" prefix so that a
        # misconfigured DB_NAME cannot accidentally affect unrelated schemas.
        if db_host and db_name:
            print("\n[4] Running database queries...")
            demo_db = f"{_PREFIX}{db_name}"

            print(f"    Using isolated demo database: '{demo_db}'")
            print(f"    (Derived from DB_NAME='{db_name}' + '{_PREFIX}' prefix)")

            # Wrap identifiers in backticks to handle names that collide with
            # MySQL reserved words or contain special characters.
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

            # Cleanup: DROP DATABASE is destructive — two independent signals
            # must both be present before we issue it.
            print("\n[6] Cleanup...")
            if not allow_drop:
                print("    ⊘ Skipping DROP DATABASE (MYSQL_SANDBOX_ALLOW_DROP not set).")
                print(f"    The demo database '{demo_db}' is left intact on the server.")
                print("    To enable auto-cleanup, run:")
                print('        export MYSQL_SANDBOX_ALLOW_DROP=1  # and use a "cube_demo_" DB_NAME')
            else:
                # Even with the env var set, refuse to drop if the database name
                # does NOT start with "cube_demo_" — catches the operator
                # accidentally setting DB_NAME=production.
                if not demo_db.startswith(_PREFIX):
                    print(
                        f"    ⊘ Refusing to DROP '{demo_db}' — name does not start with 'cube_demo_'."
                    )
                    print("    Aborting cleanup to protect unrelated data.")
                else:
                    # Safety gate: list existing databases so the operator can
                    # visually confirm before the DROP runs.
                    listing = run_mysql_query(sandbox, "SHOW DATABASES", "")
                    print(f"    Databases currently visible:\n{listing}")
                    print(f"    >>> About to DROP DATABASE: {demo_db!r} <<<")
                    try:
                        input("    Press Enter to continue or Ctrl+C to abort...")
                    except KeyboardInterrupt:
                        print("\n    Operation cancelled.")
                        return
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
