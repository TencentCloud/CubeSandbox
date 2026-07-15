#!/usr/bin/env python3
"""
run_query.py - Execute SQL queries against a MySQL database.

This script demonstrates:
1. Creating a sandbox with environment variables for DB connection
2. Running SQL queries via mysql client
3. Handling query results

Prerequisites:
- A running MySQL server accessible from the sandbox
- Set DB_HOST, DB_USER, DB_PASSWORD environment variables
"""
import sys
import os
sys.path.insert(0, str(__file__).rsplit('/', 1)[0])

from e2b_code_interpreter import Sandbox
from env_utils import get_template_id, get_api_url, get_api_key, get_ssl_cert_file, get_env

# Set environment variables
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

# Database configuration (set in .env or export)
db_host = os.environ.get("DB_HOST", "localhost")
db_user = os.environ.get("DB_USER", "root")
db_password = os.environ.get("DB_PASSWORD", "")
db_name = os.environ.get("DB_NAME", "")

print("=" * 60)
print("MySQL Client Sandbox - Execute Query")
print("=" * 60)
print(f"Template ID: {template_id}")
print(f"DB Host: {db_host}")
print(f"DB User: {db_user}")
print(f"DB Name: {db_name}")
print("=" * 60)


def build_mysql_cmd(query: str, database: str = "") -> str:
    """Build mysql command with common options."""
    db_cmd = f"mysql -h {db_host} -u {db_user}"
    if db_password:
        db_cmd += f" -p{db_password}"
    if database:
        db_cmd += f" {database}"
    db_cmd += f" -e '{query}'"
    return db_cmd


def main():
    """Main test function."""
    print("\n[1] Creating sandbox with database credentials...")

    # Create sandbox with database credentials as environment variables
    sandbox_envs = {
        "DB_HOST": db_host,
        "DB_USER": db_user,
        "DB_PASSWORD": db_password,
        "DB_NAME": db_name,
    }

    create_kwargs = {
        "template": template_id,
        "envs": sandbox_envs,
    }
    if ssl_cert:
        create_kwargs["envs"]["SSL_CERT_FILE"] = ssl_cert

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created successfully!")

        # Test 1: Check MySQL connection
        print("\n[3] Testing MySQL connection...")
        query = "SELECT 'Connection successful!' AS status"
        cmd = build_mysql_cmd(query)
        result = sandbox.commands.run(cmd)

        if result.stdout:
            print(f"    Result: {result.stdout.strip()}")
        if result.stderr:
            print(f"    Warning: {result.stderr.strip()}")

        # Test 2: Get MySQL version
        print("\n[4] Getting MySQL server version...")
        query = "SELECT VERSION() AS version"
        cmd = build_mysql_cmd(query)
        result = sandbox.commands.run(cmd)
        print(f"    {result.stdout.strip()}")

        # Test 3: List databases (if user has permission)
        print("\n[5] Listing accessible databases...")
        query = "SHOW DATABASES"
        cmd = build_mysql_cmd(query)
        result = sandbox.commands.run(cmd)
        print(f"    {result.stdout.strip()}")

        # Test 4: Run custom query (if DB_NAME is set)
        if db_name:
            print(f"\n[6] Querying database '{db_name}'...")
            query = "SHOW TABLES"
            cmd = build_mysql_cmd(query, db_name)
            result = sandbox.commands.run(cmd)
            print(f"    Tables: {result.stdout.strip() if result.stdout else '(empty)'}")
        else:
            print("\n[6] Skipping table query (DB_NAME not set)")

        print("\n" + "=" * 60)
        print("Query execution completed!")
        print("=" * 60)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"\nError: {e}")
        print("\nNote: Make sure to set DB_HOST, DB_USER, DB_PASSWORD environment variables.")
        sys.exit(1)
