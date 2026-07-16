#!/usr/bin/env python3
"""
check_mysql.py - Verify MySQL client installation in sandbox.

This script demonstrates:
1. Creating a sandbox from the MySQL template
2. Running shell commands to check MySQL client version
3. Verifying database tools are available

If MySQL client is not pre-installed, this script will:
- Attempt to install it using apt-get (Debian/Ubuntu) or yum (RHEL/CentOS)
- Verify the installation

Usage:
    python3 check_mysql.py
"""

import os
import sys

# Add the directory containing this script to Python's module search path.
# This allows importing env_utils even when the script is run from a
# different working directory.
sys.path.insert(0, str(__file__).rsplit("/", 1)[0])

from e2b_code_interpreter import Sandbox

from env_utils import get_api_key, get_api_url, get_ssl_cert_file, get_template_id

# Configure Sandbox SDK with credentials from environment variables.
# These must be set before creating any sandbox instance.
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

print("=" * 60)
print("MySQL Client Sandbox - Verification Test")
print("=" * 60)
print(f"Template ID: {template_id}")
print(f"API URL: {get_api_url()}")
print("=" * 60)


def check_mysql_in_sandbox(sandbox: Sandbox, try_install: bool = True) -> bool:
    """Check if MySQL client is available, optionally install it.

    Args:
        sandbox: Active Sandbox instance to run commands in.
        try_install: If True, attempt automatic installation when client is missing.

    Returns:
        True if MySQL client is available (either pre-installed or successfully installed).
    """
    # First check: see if mysql binary exists and is executable.
    # Non-zero exit codes are treated as "not found" and trigger install.
    # Stderr may contain the actual error (e.g. "mysql: command not found"),
    # so we include it in the message to help the user understand what happened.
    result = sandbox.commands.run("mysql --version", timeout=30)
    if result.exit_code == 0:
        print(f"    MySQL version: {result.stdout.strip()}")
        return True

    stderr = result.stderr.strip() if result.stderr else ""
    print(f"    MySQL client check failed: {stderr or 'exit code ' + str(result.exit_code)}")

    if not try_install:
        return False

    # Automatic installation: supports three major package managers.
    # The shell script exits with "INSTALL_OK" on success, which we check
    # to distinguish between successful install and a non-zero exit for other reasons.
    print("    Attempting to install MySQL client...")
    install_cmd = """
if command -v apt-get &> /dev/null; then
    # Debian/Ubuntu: use default-mysql-client for compatibility with both MySQL and MariaDB.
    apt-get update -qq && apt-get install -y -qq mysql-client default-mysql-client && echo "INSTALL_OK"
elif command -v yum &> /dev/null; then
    # RHEL/CentOS: mysql package is the official MySQL client.
    yum install -y mysql && echo "INSTALL_OK"
elif command -v apk &> /dev/null; then
    # Alpine Linux: common in minimal container images.
    apk add --no-cache mysql-client && echo "INSTALL_OK"
else
    # Unsupported OS: report error and exit cleanly.
    echo "UNSUPPORTED_PACKAGE_MANAGER" >&2
    exit 1
fi
"""
    result = sandbox.commands.run(install_cmd, timeout=300)
    if result.exit_code == 0 and "INSTALL_OK" in result.stdout:
        print("    MySQL client installed successfully!")
        # Re-verify: installation may have succeeded but the binary still
        # won't work if there are shared library issues or wrong architecture.
        result = sandbox.commands.run("mysql --version", timeout=30)
        if result.exit_code == 0:
            print(f"    MySQL version (after install): {result.stdout.strip()}")
            return True
        else:
            print("    Installation verification failed")
    else:
        # Failed installation may be due to network restrictions in the sandbox,
        # repository mirrors, or package manager issues.
        print("    Installation failed or timed out")
        stderr = result.stderr.strip() if result.stderr else ""
        if stderr:
            print(f"    Error details: {stderr}")
        print("    Note: If installation times out, the sandbox may have network restrictions.")
        print("    Consider using a pre-built template with MySQL client installed.")

    print("    MySQL client not available in this sandbox")
    return False


def main():
    """Main test function."""
    print("\n[1] Creating sandbox...")

    create_kwargs = {"template": template_id}
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created successfully!")

        # Check MySQL client (try install if not found)
        print("\n[3] Checking MySQL client...")
        mysql_available = check_mysql_in_sandbox(sandbox, try_install=True)

        if not mysql_available:
            print("\n    Note: This sandbox does not have MySQL client pre-installed.")
            print("    This is expected for base templates.")
            print("    For MySQL client support, use the mysql-sandbox template.")

        # List available database tools
        print("\n[4] Checking available database tools...")
        result = sandbox.commands.run(
            "which mysql mysqldump mysql_config 2>/dev/null || echo 'MySQL tools not found'"
        )
        print(f"    {result.stdout.strip()}")

        # Check system info
        print("\n[5] System information...")
        result = sandbox.commands.run("uname -a")
        print(f"    {result.stdout.strip()}")

        # Verify sandbox isolation
        print("\n[6] Verifying sandbox environment...")
        result = sandbox.commands.run(
            "cat /etc/os-release 2>/dev/null | head -2 || cat /etc/alpine-release 2>/dev/null || echo 'OS info not available'"
        )
        print(f"    {result.stdout.strip()}")

        print("\n" + "=" * 60)
        print("Sandbox verification completed!")
        print("=" * 60)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"\nError: {e}")
        print("\nPlease verify your configuration (API key, template ID, API URL) and try again.")
        sys.exit(1)
