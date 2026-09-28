#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Fetch an export manifest and print one field of it.

Why this exists: `layout` is only ever written into the manifest object
(s3_export.c:581). No RPC reports it -- rcow_get_lvstores describes lvols, not
exports -- so the only way to find out whether an export came out zero-copy or
copied is to read the manifest back out of S3. A test that wants to assert "this
export referenced the source rather than duplicating it" has nowhere else to look.

The S3 client is borrowed from s3_prefix_rm.py, which already implements SigV4
against the same endpoints; duplicating that signing code here would be a second
thing to keep correct.

Usage:
    s3_get_manifest.py -e <endpoint> -b <bucket> -r <region> -u <export-uuid>
                       [-p <prefix>] [--field layout] [--raw]

  -p        prefix the manifest lives under; default "exports" (the bucket-level
            directory), which is where a same-bucket export writes it
  --field   member to print; default "layout". "." prints the whole object
  --raw     print the manifest verbatim, ignoring --field

Exits non-zero if the object cannot be read, so a caller can tell "absent" from
"present but says something unexpected".
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from s3_prefix_rm import Client  # noqa: E402  (path set above)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("-e", "--endpoint", required=True)
    p.add_argument("-b", "--bucket", required=True)
    p.add_argument("-r", "--region", default="ap-nanjing")
    p.add_argument("-u", "--uuid", required=True, help="export uuid")
    p.add_argument("-p", "--prefix", default="exports",
                   help='prefix holding <uuid>.json (default: "%(default)s")')
    p.add_argument("--field", default="layout",
                   help='manifest member to print, or "." for all')
    p.add_argument("--raw", action="store_true")
    p.add_argument("--path-style", action="store_true")
    p.add_argument("--no-tls", action="store_true")
    args = p.parse_args()

    ak = os.environ.get("AWS_ACCESS_KEY_ID", "")
    sk = os.environ.get("AWS_SECRET_ACCESS_KEY", "")
    if not ak or not sk:
        print("AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY are not set",
              file=sys.stderr)
        return 2

    s3 = Client(args.endpoint, args.bucket, args.region, args.path_style, ak, sk,
            args.no_tls)
    key = "%s/%s.json" % (args.prefix.rstrip("/"), args.uuid)

    # _base_path() is empty for virtual-hosted addressing and "/<bucket>" for
    # path style; the signature covers it either way, so it cannot be skipped.
    status, body = s3.request("GET", s3._base_path() + "/" + key)
    if status != 200:
        print("GET %s -> HTTP %s" % (key, status), file=sys.stderr)
        return 1

    text = body.decode("utf-8", "replace") if isinstance(body, bytes) else body
    if args.raw:
        print(text)
        return 0

    try:
        m = json.loads(text)
    except Exception as exc:
        print("manifest is not JSON: %s" % exc, file=sys.stderr)
        return 1

    if args.field == ".":
        print(json.dumps(m, indent=2, sort_keys=True))
        return 0

    if args.field not in m:
        print("manifest has no member %r (has: %s)"
              % (args.field, ", ".join(sorted(m))), file=sys.stderr)
        return 1

    print(m[args.field])
    return 0


if __name__ == "__main__":
    sys.exit(main())
