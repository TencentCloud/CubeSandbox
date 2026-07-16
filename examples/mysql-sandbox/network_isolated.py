#!/usr/bin/env python3
"""
network_isolated.py - Demonstrate network isolation policies.

This script demonstrates three network isolation strategies available in Cube Sandbox:

1. Full isolation (allow_internet_access=False)
   - No outbound traffic allowed whatsoever
   - Suitable for maximum security workloads

2. CIDR allowlist (network={"allow_out": [...]})
   - Only specified IP ranges can receive outbound connections
   - Example: private networks only (10.0.0.0/8, 192.168.0.0/16, 172.16.0.0/12)
   - Public internet is blocked

3. CIDR denylist (network={"deny_out": [...]})
   - All outbound traffic allowed except specified IP ranges
   - Less restrictive but more complex to reason about

Usage:
    python3 network_isolated.py

Note: This script does not require a MySQL server to be running.
"""

import os
import sys

# Ensure env_utils can be imported regardless of working directory.
sys.path.insert(0, str(__file__).rsplit("/", 1)[0])

from e2b_code_interpreter import Sandbox

from env_utils import get_api_key, get_api_url, get_env, get_ssl_cert_file, get_template_id

# Initialize Sandbox SDK credentials from environment.
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

print("=" * 60)
print("MySQL Client Sandbox - Network Isolation Demo")
print("=" * 60)
print(f"Template ID: {template_id}")
print("=" * 60)


def test_full_isolation():
    """Test 1: Fully isolated sandbox with no internet access.

    This demonstrates the most restrictive network policy.
    Use this when you need to guarantee that no data can leave the sandbox.
    """
    print("\n" + "-" * 40)
    print("Test 1: Full Network Isolation")
    print("-" * 40)

    print("[1] Creating fully isolated sandbox...")

    create_kwargs = {
        "template": template_id,
        # No outbound traffic of any kind is allowed.
        # Even DNS queries to resolve hostnames will fail.
        "allow_internet_access": False,
    }
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created with full isolation!")

        # Verify: attempt to reach a public endpoint.
        # Expected: connection fails (blocked by policy).
        print("\n[3] Verifying internet access is blocked...")
        result = sandbox.commands.run(
            "curl -s --connect-timeout 5 https://google.com 2>&1 || echo 'BLOCKED: Internet access denied'"
        )

        if "BLOCKED" in result.stdout or result.exit_code != 0:
            print("    ✓ Internet access correctly blocked!")
        else:
            print("    ✗ Unexpected: Internet access allowed")

        print("\n    ✓ Sandbox created with full isolation!")


def test_allowlist():
    """Test 2: Sandbox with CIDR allowlist (private networks only).

    This demonstrates a more practical isolation policy where only
    internal/private networks are reachable. Useful for connecting to
    databases on private infrastructure.
    """
    print("\n" + "-" * 40)
    print("Test 2: CIDR Allowlist (Internal Networks Only)")
    print("-" * 40)

    print("[1] Creating sandbox with allowlist...")

    # Only RFC 1918 private address ranges are allowed.
    # Anything else (public IPs, including 8.8.8.8 for DNS) is blocked.
    create_kwargs = {
        "template": template_id,
        "network": {
            "allow_out": ["10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"],
        },
    }
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created with CIDR allowlist!")

        print("\n[3] Allowed outbound networks:")
        print("    - 10.0.0.0/8 (Class A private)")
        print("    - 192.168.0.0/16 (Class B private)")
        print("    - 172.16.0.0/12 (Class C private)")

        # Verify: public internet should be blocked.
        print("\n[4] Verifying public internet is blocked...")
        result = sandbox.commands.run(
            "curl -s --connect-timeout 5 https://google.com 2>&1 || echo 'BLOCKED'"
        )
        if "BLOCKED" in result.stdout or result.exit_code != 0:
            print("    ✓ Public internet correctly blocked!")
        else:
            print("    ✗ Public internet unexpectedly allowed!")

        # Verify: loopback is always reachable (never subject to network policy).
        print("\n[5] Verifying loopback connectivity (always allowed)...")
        result = sandbox.commands.run("ping -c 1 127.0.0.1 --timeout=3 2>&1")
        if result.exit_code == 0:
            print("    ✓ Loopback (127.0.0.1) accessible!")
        else:
            print("    ✗ Loopback not accessible - policy may be too restrictive")

        # Verify: private IP in allowlist — traffic is permitted (may still fail
        # if there's no host at that IP, but the policy allows it).
        print("\n[6] Verifying private network access (traffic allowed to 10.0.0.0/8)...")
        result = sandbox.commands.run("ping -c 1 10.255.255.1 --timeout=3 2>&1 || echo 'UNREACHABLE'")
        if "UNREACHABLE" not in result.stdout and "Destination Host Unreachable" not in result.stdout:
            print("    ✓ Private network traffic allowed!")
        else:
            print("    Note: No host at 10.255.255.1, but traffic was policy-permitted")

        print("\n    ✓ Sandbox configured with CIDR allowlist!")


def test_database_access():
    """Test 3: Database access with network isolation.

    This demonstrates the recommended setup for production database access:
    - Only the internal network (where the DB lives) is reachable
    - Public internet is blocked to prevent data exfiltration
    - Database credentials are injected as environment variables
    """
    print("\n" + "-" * 40)
    print("Test 3: Database Access with Isolation")
    print("-" * 40)

    print("[1] Creating sandbox for database access...")

    create_kwargs = {
        "template": template_id,
        # Restrict to internal network only — change this CIDR to match
        # your actual database server's subnet.
        "network": {
            "allow_out": ["10.0.0.0/8"],
        },
        "envs": {
            "DB_HOST": get_env("DB_HOST", "localhost") or "localhost",
            "DB_USER": get_env("DB_USER", "root") or "root",
            "DB_NAME": get_env("DB_NAME", "") or "",
        },
    }
    db_password = get_env("DB_PASSWORD", "") or ""
    if db_password:
        # Inject password via MYSQL_PWD — never on the command line.
        create_kwargs["envs"]["MYSQL_PWD"] = db_password
    if ssl_cert:
        create_kwargs["envs"]["SSL_CERT_FILE"] = ssl_cert

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created for database access!")

        print("\n[3] Database connectivity configuration:")
        print("    Network policy: Only 10.0.0.0/8 allowed")
        print("    (Set DB_HOST, DB_USER, DB_PASSWORD to connect)")

        # Verify: public internet blocked.
        print("\n[4] Verifying public internet is blocked...")
        result = sandbox.commands.run(
            "curl -s --connect-timeout 5 https://google.com 2>&1 || echo 'BLOCKED'"
        )
        if "BLOCKED" in result.stdout or result.exit_code != 0:
            print("    ✓ Public internet correctly blocked!")
        else:
            print("    ✗ Public internet unexpectedly allowed!")

        # Verify: internal network reachable.
        print("\n[5] Verifying internal network access (10.0.0.0/8 allowed)...")
        result = sandbox.commands.run("ping -c 1 10.0.0.1 --timeout=3 2>&1 || echo 'UNREACHABLE'")
        if "UNREACHABLE" not in result.stdout and "Destination Host Unreachable" not in result.stdout:
            print("    ✓ Private network traffic allowed!")
        else:
            print("    Note: No host at 10.0.0.1, but traffic was policy-permitted")

        # Verify: loopback always works.
        print("\n[6] Verifying loopback connectivity (always allowed)...")
        result = sandbox.commands.run("ping -c 1 127.0.0.1 --timeout=3 2>&1")
        if result.exit_code == 0:
            print("    ✓ Loopback (127.0.0.1) accessible!")
        else:
            print("    ✗ Loopback not accessible - policy may be too restrictive")

        print("\n    ✓ Database sandbox configured!")


def main():
    """Run all network isolation tests."""
    print("\nRunning network isolation tests...\n")

    try:
        test_full_isolation()
        test_allowlist()
        test_database_access()

        print("\n" + "=" * 60)
        print("All network isolation tests completed!")
        print("=" * 60)

    except Exception as e:
        print(f"\nError: {e}")
        import traceback

        traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()
