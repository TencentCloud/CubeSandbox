#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Dial a sandbox service through CubeProxy plaintext gRPC ingress (port 9091).
#
# The Python SDK's commands/files APIs use HTTP/Connect on CubeProxy HTTP/HTTPS.
# This script shows the native gRPC client path:
#   dial  <CUBE_PROXY_NODE_IP>:9091
#   set   :authority to <container_port>-<sandbox_id>
#
# Run:
#     cp .env.example .env   # fill in values
#     pip install -r requirements.txt
#     python grpc_plaintext.py

import os
import sys

import grpc
from cubesandbox import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

PROXY_IP = os.environ.get("CUBE_PROXY_NODE_IP")
GRPC_PORT = int(os.environ.get("CUBE_PROXY_GRPC_PORT", "9091"))
ENVD_PORT = int(os.environ.get("ENVD_PORT", "49983"))
TEMPLATE_ID = os.environ.get("CUBE_TEMPLATE_ID")

if not PROXY_IP:
    sys.exit("CUBE_PROXY_NODE_IP is required (CubeProxy IP reachable from this host)")
if not TEMPLATE_ID:
    sys.exit("CUBE_TEMPLATE_ID is required")


def main() -> None:
    print(f"creating sandbox from template {TEMPLATE_ID}")
    with Sandbox.create(template=TEMPLATE_ID, timeout=300) as sandbox:
        authority = f"{ENVD_PORT}-{sandbox.sandbox_id}"
        target = f"{PROXY_IP}:{GRPC_PORT}"
        print(f"dialing {target} with :authority={authority}")

        channel = grpc.insecure_channel(
            target,
            options=(("grpc.default_authority", authority),),
        )
        try:
            grpc.channel_ready_future(channel).result(timeout=15)
            print("gRPC channel is READY (CubeProxy routed the connection)")
        except grpc.FutureTimeoutError:
            sys.exit(
                f"timed out connecting to {target} "
                f"(is CubeProxy gRPC ingress listening on :{GRPC_PORT}?)"
            )
        except grpc.RpcError as exc:
            sys.exit(f"gRPC connection failed: {exc}")
        finally:
            channel.close()

    print("sandbox deleted")


if __name__ == "__main__":
    main()
