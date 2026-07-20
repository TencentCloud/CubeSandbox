#!/usr/bin/env python3
"""
MySQL Client Sandbox - Environment Check Script

This script verifies that the MySQL client sandbox template is properly configured
by creating a sandbox and checking the MySQL client environment.
"""

import os
import sys

from env_utils import get_api_key, get_api_url, get_template_id

try:
    from e2b_code_interpreter import Sandbox
except ImportError:
    print("Error: Please install dependencies first:")
    print("  pip install -r requirements.txt")
    sys.exit(1)


def main():
    print("=" * 60)
    print("MySQL Client Sandbox - Environment Check")
    print("=" * 60)

    # Load and set required environment variables
    template_id = get_template_id()
    api_url = get_api_url()
    api_key = get_api_key()

    os.environ["E2B_API_URL"] = api_url
    os.environ["E2B_API_KEY"] = api_key

    print(f"\nTemplate ID: {template_id}")
    print(f"API URL: {api_url}")
    print("=" * 60)

    # Step 1: Create sandbox
    print("\n[1] Creating sandbox...")
    try:
        sandbox = Sandbox.create(template=template_id)
        print(f"[2] Sandbox created: {sandbox.sandbox_id}")
    except Exception as e:
        print(f"Failed to create sandbox: {e}")
        sys.exit(1)

    try:
        # Step 3: Check MySQL client version
        print("\n[3] Checking MySQL client version...")
        result = sandbox.commands.run("mysql --version")
        if result.exit_code == 0:
            print(f"    MySQL version: {result.stdout.strip()}")
        else:
            print(f"    Error: {result.stderr.strip() if result.stderr else 'command failed'}")

        # Step 4: Check available database tools
        print("\n[4] Checking available database tools...")
        tools = ["mysql", "mysqldump"]
        for tool in tools:
            result = sandbox.commands.run(f"which {tool}")
            if result.exit_code == 0:
                print(f"    {result.stdout.strip()}")
            else:
                print(f"    {tool}: not found")

        # Step 5: Verify sandbox environment
        print("\n[5] Verifying sandbox environment...")
        result = sandbox.commands.run("cat /etc/os-release | grep PRETTY_NAME")
        if result.exit_code == 0:
            print(f"    {result.stdout.strip()}")

        result = sandbox.commands.run("uname -a")
        if result.exit_code == 0:
            print(f"    {result.stdout.strip()}")

        # Step 6: Display connection info
        print("\n[6] Connection Information:")
        print("    DB_HOST: localhost (use port mapping or external host)")
        print("    DB_USER: root")
        print("    DB_PORT: 3306")
        print("\n    To connect to an external MySQL server, set envs:")
        print("    Sandbox.create(template=template_id, envs={'DB_HOST': 'your-host.com'})")

        print("\n" + "=" * 60)
        print("Sandbox verification completed!")
        print("=" * 60)

    finally:
        # Clean up sandbox
        print("\n[+] Cleaning up sandbox...")
        sandbox.kill()
        print("[+] Sandbox killed.")


if __name__ == "__main__":
    main()
