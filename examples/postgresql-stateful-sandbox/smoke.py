# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Verify SQL execution and the PostgreSQL template's network isolation."""

from pathlib import Path
from typing import Optional
from urllib.error import HTTPError
from urllib.request import ProxyHandler, Request, build_opener

from cubesandbox import Config, Sandbox
from env_utils import load_local_dotenv
from postgres_utils import (
    POSTGRES_USER,
    apply_sql_file,
    create_secure_sandbox,
    cube_config,
    kill_sandbox,
    sql,
    wait_for_postgres,
)

EXAMPLE_DIR = Path(__file__).resolve().parent
ENVD_PORT = 49983


def envd_health_status(
    sandbox: Sandbox,
    config: Config,
    *,
    traffic_token: Optional[str] = None,
) -> int:
    """Read envd health with no host proxy settings and return any HTTP code."""
    headers = {}
    if traffic_token is not None:
        headers["e2b-traffic-access-token"] = traffic_token
    if config.proxy_node_ip:
        url = f"http://{config.proxy_node_ip}:{config.proxy_port}/health"
        headers["Host"] = sandbox.get_host(ENVD_PORT)
    else:
        url = f"http://{sandbox.get_host(ENVD_PORT)}/health"
    request = Request(url, headers=headers, method="GET")
    opener = build_opener(ProxyHandler({}))
    try:
        with opener.open(request, timeout=5) as response:
            return response.status
    except HTTPError as exc:
        status = exc.code
        exc.close()
        return status


def main() -> None:
    load_local_dotenv()
    config = cube_config()
    sandbox = create_secure_sandbox(config=config)
    print(f"sandbox: {sandbox.sandbox_id}")

    try:
        wait_for_postgres(sandbox)
        apply_sql_file(sandbox, EXAMPLE_DIR / "sql" / "base_schema.sql")

        server_version_num = sql(
            sandbox,
            "SELECT current_setting('server_version_num');",
        )
        if server_version_num != "160014":
            raise AssertionError(
                "expected PostgreSQL 16.14 (server_version_num 160014), got "
                f"{server_version_num!r}"
            )

        account_summary = sql(
            sandbox,
            "SELECT count(*)::text || ':' || sum(balance)::text FROM accounts;",
        )
        if account_summary != "2:300":
            raise AssertionError(
                f"expected two accounts with balance 300, got {account_summary!r}"
            )

        traffic_token = sandbox.traffic_access_token
        if not traffic_token:
            raise AssertionError("sandbox creation did not return a traffic access token")
        authenticated_status = envd_health_status(
            sandbox,
            config,
            traffic_token=traffic_token,
        )
        if authenticated_status != 204:
            raise AssertionError(
                "envd health with a traffic token returned "
                f"HTTP {authenticated_status}, expected 204"
            )

        unauthenticated_status = envd_health_status(sandbox, config)
        if unauthenticated_status == 204:
            raise AssertionError(
                "envd health without a traffic token unexpectedly returned HTTP 204"
            )

        internet = sandbox.commands.run(
            "curl --fail --insecure --silent --show-error --max-time 3 "
            "https://example.com --output /dev/null",
            timeout=5,
            user=POSTGRES_USER,
        )
        if internet.exit_code == 0:
            raise AssertionError("public internet unexpectedly remained reachable")

        if sql(sandbox, "SELECT 1;") != "1":
            raise AssertionError("local PostgreSQL stopped responding after egress denial")

        tcp = sandbox.commands.run(
            "pg_isready --quiet --timeout=3 --host=127.0.0.1 "
            "--port=5432 --username=postgres --dbname=postgres",
            timeout=5,
            user=POSTGRES_USER,
        )
        if tcp.exit_code == 0:
            raise AssertionError("PostgreSQL unexpectedly accepted TCP connections")
    finally:
        kill_sandbox(sandbox)

    print("OK: PostgreSQL template is ready and network isolation is enforced")


if __name__ == "__main__":
    main()
