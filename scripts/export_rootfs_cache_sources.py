#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Read-only export of recoverable S3 rootfs sources for Cubelet cache GC."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
from urllib.parse import urlencode, urlsplit


SQL = """
SELECT JSON_OBJECT('artifact_id', artifact_id, 'token', download_token,
                  'sha256', ext4_sha256, 'size_bytes', ext4_size_bytes)
FROM t_cube_rootfs_artifact
WHERE deleted_at IS NULL AND status = 'READY' AND LOWER(storage_backend) = 's3'
  AND ext4_size_bytes > 0 AND LENGTH(ext4_sha256) = 64 AND download_token <> '';
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", default="cube-system")
    parser.add_argument("--pod", default="cubesandbox-mysql-0")
    parser.add_argument("--master-url", required=True,
                        help="Stable CubeMaster HTTP origin reachable from workers")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    origin = urlsplit(args.master_url)
    if (origin.scheme not in ("http", "https") or not origin.netloc
            or origin.username or origin.password or origin.query or origin.fragment
            or origin.path not in ("", "/")):
        parser.error("--master-url must be an HTTP origin without credentials or a path")
    if not args.output.is_absolute():
        parser.error("--output must be an absolute path")
    # Credentials stay in the existing MySQL pod environment, never in argv.
    command = ["kubectl", "exec", "-i", "-n", args.namespace, args.pod, "--",
               "sh", "-c", 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --user=root '
               '--database="$MYSQL_DATABASE" --batch --raw --skip-column-names']
    try:
        result = subprocess.run(command, input=SQL, text=True, encoding="utf-8",
                                capture_output=True, check=True, timeout=60)
    except (OSError, subprocess.SubprocessError):
        parser.exit(1, "Read-only source export failed; check kubectl and MySQL pod access.\n")
    artifacts = []
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        row.update(instance_type="cubebox", remote_backed=True,
                   url=args.master_url.rstrip("/") + "/cube/template/artifact/download?"
                   + urlencode({"artifact_id": row["artifact_id"]}))
        artifacts.append(row)
    fd, temporary = tempfile.mkstemp(prefix=".rootfs-sources-", dir=args.output.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            json.dump({"version": 1, "artifacts": artifacts}, output, ensure_ascii=False)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, args.output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    print(f"Exported {len(artifacts)} artifact sources; protect this file as a credential.")


if __name__ == "__main__":
    main()
