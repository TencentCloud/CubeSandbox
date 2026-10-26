#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import os
import sys

import httpx
from _pi_common import sandbox_identifier
from e2b import Sandbox
from env_utils import (
    int_env,
    load_local_dotenv,
    optional,
    pi_model,
    pi_provider,
    require_provider_key,
    required,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Send a task to the pre-warmed Pi AgentSession in CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_WARMUP_TEMPLATE_ID"),
        help="Warmup template ID. Defaults to CUBE_WARMUP_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--prompt",
        default="Create hello.py that prints 'Hello from pre-warmed Pi' and run it.",
        help="Prompt sent to the resident Pi SDK adapter.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("PI_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds.",
    )
    parser.add_argument(
        "--request-timeout",
        type=int,
        default=int_env("PI_AGENT_EXEC_TIMEOUT", 900),
        help="HTTP task timeout in seconds.",
    )
    return parser.parse_args()


def adapter_url(sandbox: Sandbox) -> str:
    return f"http://{sandbox.get_host(8080)}"


def print_messages(messages: object) -> None:
    if not isinstance(messages, list):
        return
    for message in messages:
        if not isinstance(message, dict) or message.get("role") != "assistant":
            continue
        content = message.get("content")
        if isinstance(content, str) and content.strip():
            print(content.strip())
        elif isinstance(content, list):
            for item in content:
                if isinstance(item, dict) and item.get("type") == "text":
                    text = str(item.get("text", "")).strip()
                    if text:
                        print(text)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    template_id = args.template or required("CUBE_WARMUP_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    provider = pi_provider()
    api_key = require_provider_key(provider)
    provider_base_url = (
        optional("ANTHROPIC_BASE_URL") if provider == "anthropic" else ""
    )

    print(f"Creating sandbox from warmup template: {template_id}")
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        print(f"Sandbox ready: {sandbox_identifier(sandbox)}")
        base_url = adapter_url(sandbox)
        timeout = httpx.Timeout(args.request_timeout, connect=30)
        with httpx.Client(timeout=timeout) as client:
            ready_response = client.get(f"{base_url}/readyz")
            ready_response.raise_for_status()
            print(f"Pre-warmed session: {ready_response.json()}")

            response = client.post(
                f"{base_url}/prompt",
                json={
                    "prompt": args.prompt,
                    "provider": provider,
                    "model": pi_model(),
                    "apiKey": api_key,
                    **({"baseUrl": provider_base_url} if provider_base_url else {}),
                },
            )
            response.raise_for_status()
            result = response.json()
            print(f"Pi session ID: {result.get('sessionId', 'unknown')}")
            print_messages(result.get("messages"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
