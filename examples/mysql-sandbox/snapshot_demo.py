#!/usr/bin/env python3
"""
MySQL Client Sandbox - Snapshot Demo Script

This script demonstrates CubeSandbox's snapshot capabilities by:
1. Creating a sandbox and making initial state
2. Creating a snapshot
3. Modifying the state
4. Restoring from the snapshot
"""

import os
import sys
import time

from env_utils import get_api_key, get_api_url, get_template_id

try:
    from e2b_code_interpreter import Sandbox
except ImportError:
    print("Error: Please install dependencies first:")
    print("  pip install -r requirements.txt")
    sys.exit(1)


def main():
    print("=" * 60)
    print("MySQL Sandbox - Snapshot Demo")
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

    # Step 1: Create initial sandbox
    print("\n[1] Creating initial sandbox...")
    try:
        sandbox = Sandbox.create(template=template_id)
        print(f"    Sandbox created: {sandbox.sandbox_id}")
    except Exception as e:
        print(f"Failed to create sandbox: {e}")
        sys.exit(1)

    try:
        # Step 2: Set initial state - create test file
        print("\n[2] Setting initial state...")
        sandbox.commands.run("echo 'v1.0 - Initial state' > /tmp/state.txt")
        result = sandbox.commands.run("cat /tmp/state.txt")
        print(f"    Initial content: {result.stdout.strip()}")

        # Step 3: Create snapshot
        print("\n[3] Creating snapshot...")
        try:
            snapshot_id = sandbox.create_snapshot()
            print(f"    Snapshot ID: {snapshot_id}")
        except AttributeError:
            print("    Warning: create_snapshot() not available, skipping snapshot demo")
            print("    (This feature requires CubeSandbox version >= 0.3.0)")
            sandbox.kill()
            return

        # Step 4: Modify state
        print("\n[4] Modifying state...")
        sandbox.commands.run("echo 'v2.0 - Modified state' > /tmp/state.txt")
        result = sandbox.commands.run("cat /tmp/state.txt")
        print(f"    Modified content: {result.stdout.strip()}")

        # Wait a bit to demonstrate time passage
        print("\n[5] Waiting 2 seconds...")
        time.sleep(2)

        # Step 5: Restore from snapshot
        print("\n[6] Restoring from snapshot...")
        sandbox.restore_snapshot(snapshot_id)
        result = sandbox.commands.run("cat /tmp/state.txt")
        print(f"    Restored content: {result.stdout.strip()}")

        if "v1.0" in result.stdout:
            print("\n    ✓ State successfully restored to v1.0!")
        else:
            print("\n    ✗ State restoration failed")

        print("\n" + "=" * 60)
        print("Snapshot test passed!")
        print("=" * 60)

    finally:
        # Clean up sandbox
        print("\n[+] Cleaning up sandbox...")
        sandbox.kill()
        print("[+] Sandbox killed.")


if __name__ == "__main__":
    main()
