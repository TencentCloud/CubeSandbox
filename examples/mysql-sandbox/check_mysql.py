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
"""

import os
import sys

sys.path.insert(0, str(__file__).rsplit("/", 1)[0])

from e2b_code_interpreter import Sandbox

from env_utils import get_api_key, get_api_url, get_ssl_cert_file, get_template_id

# Set environment variables for the sandbox
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
    """Check if MySQL client is available, optionally install it."""
    result = sandbox.commands.run("mysql --version", timeout=30)
    if result.exit_code == 0:
        print(f"    MySQL version: {result.stdout.strip()}")
        return True
    # Non-zero exit codes are treated as "not found" and trigger install.
    # Stderr may contain the actual error (e.g. "mysql: command not found"),
    # so we include it in the message to help the user understand what happened.
    stderr = result.stderr.strip() if result.stderr else ""
    print(f"    MySQL client check failed: {stderr or 'exit code ' + str(result.exit_code)}")

    if try_install:
        print("    Attempting to install MySQL client...")
        install_cmd = """
if command -v apt-get &> /dev/null; then
    apt-get update -qq && apt-get install -y -qq mysql-client default-mysql-client && echo "INSTALL_OK"
elif command -v yum &> /dev/null; then
    yum install -y mysql && echo "INSTALL_OK"
elif command -v apk &> /dev/null; then
    apk add --no-cache mysql-client && echo "INSTALL_OK"
else
    echo "UNSUPPORTED_PACKAGE_MANAGER" >&2
    exit 1
fi
"""
        result = sandbox.commands.run(install_cmd, timeout=300)
        if result.exit_code == 0 and "INSTALL_OK" in result.stdout:
            print("    MySQL client installed successfully!")
            # Verify installation
            result = sandbox.commands.run("mysql --version", timeout=30)
            if result.exit_code == 0:
                print(f"    MySQL version (after install): {result.stdout.strip()}")
                return True
            else:
                print("    Installation verification failed")
        else:
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
