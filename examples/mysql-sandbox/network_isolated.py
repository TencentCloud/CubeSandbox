#!/usr/bin/env python3
"""
MySQL Client Sandbox - Network Isolation Demo Script

This script demonstrates CubeSandbox's network isolation capabilities
by creating a sandbox with no outbound internet access.
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
    print("Network Isolation Demo")
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

    # Step 1: Create sandbox with network isolation
    print("\n[1] Creating sandbox with NO internet access...")
    try:
        sandbox = Sandbox.create(
            template=template_id,
            allow_internet_access=False  # Network isolation enabled
        )
        print(f"[2] Sandbox created: {sandbox.sandbox_id}")
        print("    Network policy: allow_internet_access=False")
    except Exception as e:
        print(f"Failed to create sandbox: {e}")
        sys.exit(1)

    try:
        # Step 2: Test that internet is blocked
        print("\n[3] Testing network isolation...")
        print("    Attempting to reach google.com...")

        result = sandbox.commands.run("curl -s --connect-timeout 5 https://www.google.com || echo 'BLOCKED'")

        if "BLOCKED" in result.stdout or result.exit_code != 0:
            print("    ✓ Network isolation verified: External access blocked")
        else:
            print("    ✗ Warning: External access appears to be allowed")

        # Step 3: Verify local tools still work
        print("\n[4] Verifying local tools still work...")
        result = sandbox.commands.run("mysql --version")
        if result.exit_code == 0:
            print(f"    ✓ MySQL client: {result.stdout.strip()}")
        else:
            print("    ✗ MySQL client not working")

        result = sandbox.commands.run("echo 'Local network works!'")
        print(f"    ✓ Local commands: {result.stdout.strip()}")

        # Step 4: Explain use case
        print("\n[5] Use case explanation:")
        print("    This sandbox can connect to:")
        print("    - External MySQL servers via explicit allowlist")
        print("    - Internal databases behind VPN/private network")
        print("    - Any explicitly permitted network destinations")
        print("\n    This sandbox CANNOT connect to:")
        print("    - Random external websites")
        print("    - Unauthorized services")
        print("    - Untrusted third-party APIs")

        print("\n" + "=" * 60)
        print("Network isolation test completed!")
        print("=" * 60)

    finally:
        # Clean up sandbox
        print("\n[+] Cleaning up sandbox...")
        sandbox.kill()
        print("[+] Sandbox killed.")


if __name__ == "__main__":
    main()
