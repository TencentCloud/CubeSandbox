#!/usr/bin/env python3
"""对单个 envd 端点执行对照场景，产出可对比的 fixture JSON。

用法：
  python3 capture.py --endpoint http://127.0.0.1:49984 --implementation cube-envd \
      --out fixtures/cube-envd.json
  python3 capture.py --endpoint http://127.0.0.1:49985 --implementation go-envd \
      --out fixtures/go-envd.json

两个实现必须用**同一份** scenarios.py 采集，否则 diff 没有意义。
"""

from __future__ import annotations

import argparse
import datetime
import getpass
import json
import sys
from pathlib import Path
from typing import Any

from connect_client import Client
from scenarios import SCENARIOS


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """解析命令行参数。"""
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--endpoint", required=True, help="envd 端点，例如 http://127.0.0.1:49983")
    parser.add_argument("--out", required=True, help="fixture 输出路径")
    parser.add_argument("--implementation", default="unknown", help="实现标签（写入 fixture，便于阅读）")
    parser.add_argument(
        "--scenario",
        action="append",
        default=None,
        help="只跑指定场景（可重复）；省略表示全部",
    )
    parser.add_argument("--timeout", type=float, default=60.0, help="单请求超时秒数")
    parser.add_argument(
        "--user",
        default=None,
        help="请求要执行的本地用户（默认当前用户）；两个实现必须用同一个取值",
    )
    return parser.parse_args(argv)


def capture(
    endpoint: str, implementation: str, names: list[str], timeout: float, user: str | None
) -> dict[str, Any]:
    """执行选中的场景并返回 fixture 结构。"""
    client = Client(endpoint, timeout=timeout, user=user)
    records: dict[str, Any] = {}
    for name in names:
        scenario = SCENARIOS[name]
        try:
            result = scenario(client)
        except Exception as exc:  # 采集失败本身也要成为可对比的记录
            result = {
                "error": {
                    "type": type(exc).__name__,
                    "message": str(exc)[:500],
                }
            }
        for key, value in result.items():
            records[f"{name}/{key}"] = value
        print(f"  {name}: {len(result)} record(s)", file=sys.stderr)

    return {
        "implementation": implementation,
        "endpoint": endpoint,
        "user": user,
        "captured_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "records": dict(sorted(records.items())),
    }


def main(argv: list[str] | None = None) -> int:
    """入口：采集并写入 fixture。"""
    args = parse_args(argv)
    names = args.scenario or list(SCENARIOS)
    unknown = [name for name in names if name not in SCENARIOS]
    if unknown:
        print(f"unknown scenario(s): {', '.join(unknown)}", file=sys.stderr)
        print(f"available: {', '.join(SCENARIOS)}", file=sys.stderr)
        return 2

    print(f"capturing {len(names)} scenario(s) from {args.endpoint}", file=sys.stderr)
    user = args.user or getpass.getuser()
    fixture = capture(args.endpoint, args.implementation, names, args.timeout, user)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(fixture, indent=1, sort_keys=True, ensure_ascii=False) + "\n")
    print(f"wrote {out} ({len(fixture['records'])} records)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
