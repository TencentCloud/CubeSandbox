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
"""
import sys
import os
sys.path.insert(0, str(__file__).rsplit('/', 1)[0])

from e2b_code_interpreter import Sandbox
from env_utils import get_template_id, get_api_url, get_api_key, get_ssl_cert_file

# Set environment variables for the sandbox
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

# Database configuration
db_host = os.environ.get("DB_HOST", "localhost")
db_user = os.environ.get("DB_USER", "root")
db_password = os.environ.get("DB_PASSWORD", "")
db_name = os.environ.get("DB_NAME", "")

print("=" * 60)
print("MySQL Client Sandbox - Multi-Query Execution")
print("=" * 60)
print(f"Template ID: {template_id}")
print(f"DB Host: {db_host}")
print(f"DB User: {db_user}")
print(f"DB Name: {db_name}")
print("=" * 60)


def run_query(sandbox, query: str, database: str = "") -> str:
    """Run a query and return results."""
    cmd = f"mysql -h {db_host} -u {db_user}"
    if db_password:
        cmd += f" -p{db_password}"
    if database:
        cmd += f" {database}"
    cmd += f" -e '{query}'"

    result = sandbox.commands.run(cmd)
    if result.stderr:
        print(f"    Warning: {result.stderr.strip()}")
    return result.stdout


def main():
    """Execute multiple queries."""
    print("\n[1] Creating sandbox...")

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
            print(f"    {result.stdout.strip()}")

        # If database is configured, run actual queries
        if db_host and db_name:
            print("\n[4] Running database queries...")

            queries = [
                ("Create test database", f"CREATE DATABASE IF NOT EXISTS {db_name}"),
                ("Use database", f"USE {db_name}"),
                ("Create users table", """
CREATE TABLE IF NOT EXISTS users (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    email VARCHAR(100),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)"""),
                ("Insert user Alice", "INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com')"),
                ("Insert user Bob", "INSERT INTO users (name, email) VALUES ('Bob', 'bob@example.com')"),
                ("Select all users", "SELECT * FROM users"),
            ]

            for title, query in queries:
                print(f"\n    {title}:")
                result = run_query(sandbox, query, db_name)
                if result:
                    print(f"    {result.strip()}")

            print("\n[5] Query results summary:")
            result = run_query(sandbox, "SELECT COUNT(*) as count FROM users", db_name)
            print(f"    Total users: {result.strip()}")

            # Cleanup
            print("\n[6] Cleanup...")
            run_query(sandbox, f"DROP DATABASE IF EXISTS {db_name}", "")
            print("    ✓ Test database cleaned up")
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
