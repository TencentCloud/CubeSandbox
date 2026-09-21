#!/usr/bin/env python3
"""活体对拍：在**真实部署**的两个沙箱之间跑一致性对照。

与容器模式的差别只在"端点怎么来的"：
  * 容器模式：同一个镜像起两个容器，直连 `:49983`（`capture.py --endpoint`）；
  * 活体模式（本脚本）：经 CubeAPI 用两个模板各建一个私有沙箱，再经 CubeProxy
    （虚拟 Host + traffic token）采集，因此多覆盖了代理路由、token 校验与
    真实的模板/guest 组合。

用法：
  python3 run_live.py --api-url http://100.64.0.34:3000 --proxy-url http://100.64.0.34 \
      --domain cube.app \
      --cube-template tpl-c7a6aada633b48f79425d95f \
      --go-template tpl-5b10b52198304d698ea9c3f8 \
      --user user --outdir /tmp/live-conformance

两个模板必须来自**同一个镜像**（差别只在 `ENVD_BIN`），否则差异里会混入镜像差异。
默认跳过 `limits_probe`：它要发 65 MiB 的单帧，经代理只会撞客户端超时，测到的是网络
而不是协议——该场景由容器模式的运行覆盖。
"""

from __future__ import annotations

import argparse
import json
import pathlib
import subprocess
import sys
import time

import requests

HERE = pathlib.Path(__file__).resolve().parent
SKIP_BY_DEFAULT = ["limits_probe"]


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """解析命令行参数。"""
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--api-url", default="http://127.0.0.1:3000")
    parser.add_argument("--proxy-url", default="http://127.0.0.1")
    parser.add_argument("--domain", default="cube.app")
    parser.add_argument("--cube-template", required=True, help="跑 cube-envd 的模板 ID")
    parser.add_argument("--go-template", required=True, help="跑 Go 基线 envd 的模板 ID（同镜像 + ENVD_BIN）")
    parser.add_argument("--user", default="user", help="请求要执行的本地用户")
    parser.add_argument("--timeout", type=float, default=120.0, help="单请求超时秒数")
    parser.add_argument("--outdir", default="/tmp/live-conformance")
    parser.add_argument(
        "--scenario",
        action="append",
        default=None,
        help="只跑指定场景（可重复）；默认跑全部，且跳过 limits_probe",
    )
    parser.add_argument("--keep-sandboxes", action="store_true", help="失败时保留沙箱便于排查")
    return parser.parse_args(argv)


def create_sandbox(api_url: str, template_id: str) -> tuple[str, str]:
    """创建私有沙箱（私有才会返回 traffic token），返回 (sandboxID, token)。"""
    response = requests.post(
        f"{api_url.rstrip('/')}/sandboxes",
        json={"templateID": template_id, "network": {"allowPublicTraffic": False}},
        timeout=180,
    )
    response.raise_for_status()
    payload = response.json()
    sandbox_id = payload.get("sandboxID")
    token = payload.get("trafficAccessToken")
    if not isinstance(sandbox_id, str) or not sandbox_id:
        raise SystemExit(f"create response lacks sandboxID: {payload}")
    if not isinstance(token, str) or not token:
        raise SystemExit(f"private sandbox response lacks trafficAccessToken: {payload}")
    return sandbox_id, token


def delete_sandbox(api_url: str, sandbox_id: str) -> None:
    """删除沙箱；404 视为已删除。"""
    response = requests.delete(f"{api_url.rstrip('/')}/sandboxes/{sandbox_id}", timeout=120)
    if response.status_code not in (200, 202, 204, 404):
        print(f"warning: delete {sandbox_id} -> {response.status_code}", file=sys.stderr)


def wait_for_health(proxy_url: str, domain: str, sandbox_id: str, token: str, timeout: float = 120.0) -> None:
    """等 CubeProxy 后面的 envd 就绪。"""
    host = f"49983-{sandbox_id}.{domain}"
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        try:
            response = requests.get(
                f"{proxy_url.rstrip('/')}/health",
                headers={"Host": host, "e2b-traffic-access-token": token, "Connection": "close"},
                timeout=15,
            )
            if response.status_code == 204:
                return
            last = f"HTTP {response.status_code}"
        except requests.RequestException as exc:
            last = str(exc)
        time.sleep(1)
    raise SystemExit(f"sandbox {sandbox_id} never became healthy: {last}")


def run_capture(
    args: argparse.Namespace, label: str, template: str, scenarios: list[str], created: list[str]
) -> str:
    """建沙箱、采集 fixture，返回 fixture 路径。

    `created` 在创建成功后**立刻**登记：健康等待或采集阶段失败时，调用方的 finally
    仍然能删掉这个沙箱。此前是先采集成功再登记，结果是"沙箱起来了但采集失败"这一
    最常见的情形会把沙箱留在远端。
    """
    sandbox_id, token = create_sandbox(args.api_url, template)
    created.append(sandbox_id)
    print(f"[{label}] sandbox {sandbox_id} (template {template})", file=sys.stderr)
    wait_for_health(args.proxy_url, args.domain, sandbox_id, token)
    output = str(pathlib.Path(args.outdir) / f"{label}.json")
    command = [
        sys.executable,
        str(HERE / "capture.py"),
        "--endpoint",
        args.proxy_url,
        "--implementation",
        label,
        "--user",
        args.user,
        "--host-header",
        f"49983-{sandbox_id}.{args.domain}",
        "--token",
        token,
        "--out",
        output,
        "--timeout",
        str(args.timeout),
    ]
    for name in scenarios:
        command += ["--scenario", name]
    subprocess.run(command, check=True)
    return output


def main(argv: list[str] | None = None) -> int:
    """入口：两个活体沙箱 → 采集 → 对拍 → 清理。"""
    args = parse_args(argv)
    pathlib.Path(args.outdir).mkdir(parents=True, exist_ok=True)
    sys.path.insert(0, str(HERE))
    from scenarios import SCENARIOS  # 延迟导入：先建 outdir 再进脚本目录

    scenarios = args.scenario or [name for name in SCENARIOS if name not in SKIP_BY_DEFAULT]

    created: list[str] = []
    failed = False
    try:
        cube_fixture = run_capture(args, "cube-envd", args.cube_template, scenarios, created)
        go_fixture = run_capture(args, "go-envd", args.go_template, scenarios, created)

        results = str(pathlib.Path(args.outdir) / "RESULTS.md")
        completed = subprocess.run(
            [
                sys.executable,
                str(HERE / "conformance.py"),
                "diff",
                "--baseline",
                go_fixture,
                "--candidate",
                cube_fixture,
                "--results",
                results,
                "--strict",
            ]
        )
        failed = completed.returncode != 0
        print(f"results: {results}", file=sys.stderr)
    finally:
        if failed and args.keep_sandboxes:
            print(f"keeping sandboxes for triage: {created}", file=sys.stderr)
        else:
            for sandbox_id in created:
                delete_sandbox(args.api_url, sandbox_id)
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
