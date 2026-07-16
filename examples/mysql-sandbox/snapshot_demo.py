#!/usr/bin/env python3
"""
snapshot_demo.py - Demonstrate snapshot and rollback capabilities.

Cube Sandbox snapshots capture the entire state of a sandbox (memory + filesystem)
and persist them even after the sandbox is deleted. This enables:

- **Safe experimentation**: snapshot before risky operations, restore on failure
- **Long-running task checkpoints**: save progress and resume later
- **Branching workflows**: create multiple sandboxes from the same baseline
- **Database migration safety**: snapshot before migration, rollback if it fails

Usage:
    python3 snapshot_demo.py

Note: Snapshots persist independently of sandbox lifetime. They survive sandbox
deletion and can be used to create new sandboxes at any time.
"""

import os
import sys

sys.path.insert(0, str(__file__).rsplit("/", 1)[0])

from e2b_code_interpreter import Sandbox

from env_utils import get_api_key, get_api_url, get_env, get_ssl_cert_file, get_template_id

# Initialize Sandbox SDK credentials.
os.environ["E2B_API_URL"] = get_api_url()
os.environ["E2B_API_KEY"] = get_api_key()

template_id = get_template_id()
ssl_cert = get_ssl_cert_file()

print("=" * 60)
print("MySQL Client Sandbox - Snapshot & Rollback Demo")
print("=" * 60)
print(f"Template ID: {template_id}")
print("=" * 60)


def demo_snapshot_workflow():
    """Demonstrate basic snapshot workflow: create, modify, rollback.

    This demo shows:
    1. Create a sandbox and write some initial state
    2. Create a snapshot (captures current state)
    3. Modify the state (simulating a risky operation)
    4. Create a NEW sandbox from the snapshot — it has the original state
    """
    print("\n" + "-" * 40)
    print("Snapshot Workflow Demo")
    print("-" * 40)

    print("\n[1] Creating initial sandbox...")

    create_kwargs = {"template": template_id}
    if ssl_cert:
        create_kwargs["envs"] = {"SSL_CERT_FILE": ssl_cert}

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created!")

        # Step 1: Write some initial state to a file.
        print("\n[3] Setting up initial state...")
        sandbox.commands.run("echo 'initial state' > /tmp/app_state.txt")
        sandbox.commands.run("echo 'version: 1.0' >> /tmp/app_state.txt")

        result = sandbox.commands.run("cat /tmp/app_state.txt")
        print(f"    Initial state:\n{result.stdout}")

        # Step 2: Snapshot the current state.
        # The snapshot captures memory + filesystem at this moment.
        print("\n[4] Creating snapshot of initial state...")
        try:
            snapshot = sandbox.create_snapshot()
            snapshot_id = snapshot.snapshot_id
            print(f"    ✓ Snapshot created: {snapshot_id}")
        except Exception as e:
            # Snapshot creation may fail due to resource limits or permissions.
            print(f"    Note: Snapshot failed: {e}")
            return

        # Step 3: Modify the state — simulating a risky operation.
        print("\n[5] Modifying state (simulating dangerous operation)...")
        sandbox.commands.run("echo 'version: 2.0' > /tmp/app_state.txt")
        sandbox.commands.run("echo 'modified by user' >> /tmp/app_state.txt")

        result = sandbox.commands.run("cat /tmp/app_state.txt")
        print(f"    Modified state:\n{result.stdout}")

    # Step 4: Create a NEW sandbox from the snapshot.
    # This sandbox starts in the exact state from when the snapshot was taken.
    print(f"\n[6] Creating new sandbox from snapshot {snapshot_id[:20]}...")

    try:
        with Sandbox.create(snapshot_id) as restored_sandbox:
            result = restored_sandbox.commands.run("cat /tmp/app_state.txt")
            print(f"    State in restored sandbox:\n{result.stdout}")

            if "initial state" in result.stdout and "version: 1.0" in result.stdout:
                print("\n    ✓ Rollback successful! State restored from snapshot.")
            else:
                print("\n    Note: State differs from expected.")
    except Exception as e:
        print(f"    Note: Could not restore from snapshot: {e}")


def demo_safe_database_operations():
    """Demonstrate snapshot-based safety for database migrations.

    This demo shows how to safely test database changes:
    1. Snapshot the current database schema (export via mysqldump)
    2. Apply the migration
    3. If the migration fails or causes issues, restore from snapshot

    Note: This snapshots the sandbox filesystem, not the database itself.
    For full database snapshot/restore, use database-native tools like
    mysqldump or point-in-time recovery.
    """
    print("\n" + "-" * 40)
    print("Safe Database Operations Demo")
    print("-" * 40)

    print("\n[1] Creating sandbox for database operations...")

    create_kwargs = {
        "template": template_id,
        "envs": {
            "DB_HOST": get_env("DB_HOST", "localhost") or "localhost",
            "DB_USER": get_env("DB_USER", "root") or "root",
            "DB_NAME": get_env("DB_NAME", "") or "",
        },
    }
    db_password = get_env("DB_PASSWORD", "") or ""
    if db_password:
        create_kwargs["envs"]["MYSQL_PWD"] = db_password
    if ssl_cert:
        create_kwargs["envs"]["SSL_CERT_FILE"] = ssl_cert

    with Sandbox.create(**create_kwargs) as sandbox:
        print("[2] Sandbox created!")

        # Setup a workspace directory for SQL files.
        print("\n[3] Preparing SQL workspace...")
        sandbox.commands.run("mkdir -p /srv/sql")

        # Save initial schema to a file (simple export without mysqldump).
        print("\n[4] Saving initial schema...")
        sandbox.commands.run("""
cat > /srv/sql/schema.sql << 'EOF'
-- Initial schema
CREATE TABLE IF NOT EXISTS products (
    id INT PRIMARY KEY,
    name VARCHAR(100)
);
INSERT INTO products VALUES (1, 'Widget');
EOF
""")
        print("    ✓ Schema saved")

        # Create a snapshot before applying any changes.
        print("\n[5] Creating pre-operation snapshot...")
        try:
            pre_snapshot = sandbox.create_snapshot()
            print(f"    ✓ Pre-operation snapshot: {pre_snapshot.snapshot_id[:20]}...")
        except Exception as e:
            print(f"    Note: {e}")
            return

        # Prepare the migration script.
        print("\n[6] Preparing schema migration script...")
        sandbox.commands.run("""
cat > /srv/sql/migration.sql << 'EOF'
-- Risky migration: add a new column
ALTER TABLE products ADD COLUMN description TEXT;
EOF
""")
        print("    ✓ Migration script prepared")

    # Restore from snapshot (rollback to safe state).
    print("\n[7] Rolling back to safe state...")
    try:
        with Sandbox.create(pre_snapshot.snapshot_id) as restored:
            result = restored.commands.run("cat /srv/sql/schema.sql | head -5")
            print(f"    ✓ Schema restored:\n{result.stdout}")
            print("\n    ✓ Safe database operation workflow complete!")
    except Exception as e:
        print(f"    Note: {e}")


def demo_list_snapshots():
    """Demonstrate listing and using existing snapshots.

    This shows how to enumerate all snapshots in the current namespace
    and how to create a new sandbox from any of them.
    """
    print("\n" + "-" * 40)
    print("List & Use Snapshots Demo")
    print("-" * 40)

    print("\n[1] Listing existing snapshots...")

    try:
        # list_snapshots() returns a tuple: (list of SnapshotInfo objects, next_token).
        # The next_token is used for pagination when many snapshots exist.
        snapshots, next_token = Sandbox.list_snapshots()
        print(f"    Found {len(snapshots)} snapshot(s)")

        for snap in snapshots:
            print(f"\n    Snapshot ID: {snap.snapshot_id}")
            if snap.names:
                print(f"    Names: {', '.join(snap.names)}")

        if snapshots:
            print("\n[2] You can create a new sandbox from any snapshot using:")
            print(f"    Sandbox.create('{snapshots[0].snapshot_id}')")
    except Exception as e:
        print(f"    Note: Could not list snapshots: {e}")

    print("\n    ✓ Snapshot listing demo complete!")


def main():
    """Run all snapshot demonstrations."""
    print("\nRunning snapshot demonstrations...\n")

    try:
        demo_snapshot_workflow()
        demo_safe_database_operations()
        demo_list_snapshots()

        print("\n" + "=" * 60)
        print("All snapshot demonstrations completed!")
        print("=" * 60)

    except Exception as e:
        print(f"\nError: {e}")
        import traceback

        traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()
