#!/usr/bin/env python3
"""
network_isolated.py - Demonstrate network isolation policies.

This script demonstrates:
1. Creating a sandbox with network allowlist (only specific CIDRs allowed)
2. Creating a fully isolated sandbox (no internet access)
3. Testing network policies

Network Policy Options:
- allow_internet_access=False: Completely isolated, no outbound traffic
- network={"allow_out": ["10.0.0.0/8"]}: Only allow specific private networks
- network={"deny_out": ["1.2.3.4/32"]}: Block specific IP/CIDR
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

print("=" * 60)
print("MySQL Client Sandbox - Network Isolation Demo")
print("=" * 60)
print(f"Template ID: {template_id}")
print("=" * 60)


def test_full_isolation():
    """Test fully isolated sandbox (no internet)."""
    print("\n" + "-" * 40)
    print("Test 1: Full Network Isolation")
    print("-" * 40)

    print("[1] Creating fully isolated sandbox...")

    create_kwargs = {
        "template": template_id,
        "allow_internet_access": False,  # No internet access
    }
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created with full isolation!")

        # Try to access internet - should fail
        print("\n[3] Testing internet access (should be blocked)...")
        result = sandbox.commands.run("curl -s --connect-timeout 5 https://google.com 2>&1 || echo 'BLOCKED: Internet access denied'")

        if "BLOCKED" in result.stdout or result.exit_code != 0:
            print("    ✓ Internet access correctly blocked!")
        else:
            print("    ✗ Unexpected: Internet access allowed")

        print("\n    ✓ Sandbox created with full isolation!")


def test_allowlist():
    """Test sandbox with CIDR allowlist."""
    print("\n" + "-" * 40)
    print("Test 2: CIDR Allowlist (Internal Networks Only)")
    print("-" * 40)

    print("[1] Creating sandbox with allowlist...")

    # Use network parameter for allow_out
    create_kwargs = {
        "template": template_id,
        "network": {
            "allow_out": ["10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"],  # Private networks
        },
    }
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created with CIDR allowlist!")

        # Show allowed networks
        print("\n[3] Allowed outbound networks:")
        print("    - 10.0.0.0/8 (Class A private)")
        print("    - 192.168.0.0/16 (Class B private)")
        print("    - 172.16.0.0/12 (Class C private)")

        print("\n    ✓ Sandbox configured with CIDR allowlist!")


def test_database_access():
    """Test database access with network isolation."""
    print("\n" + "-" * 40)
    print("Test 3: Database Access with Isolation")
    print("-" * 40)

    print("[1] Creating sandbox for database access...")

    # Example: Allow only database network
    create_kwargs = {
        "template": template_id,
        "network": {
            "allow_out": ["10.0.0.0/8"],  # Only internal network
        },
        "envs": {
            "DB_HOST": os.environ.get("DB_HOST", "localhost"),
            "DB_USER": os.environ.get("DB_USER", "root"),
            "DB_PASSWORD": os.environ.get("DB_PASSWORD", ""),
        },
    }
    if ssl_cert:
        create_kwargs["envs"]["SSL_CERT_FILE"] = ssl_cert

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created for database access!")

        print("\n[3] Database connectivity configuration:")
        print("    Network policy: Only 10.0.0.0/8 allowed")
        print("    (Set DB_HOST, DB_USER, DB_PASSWORD to connect)")

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
