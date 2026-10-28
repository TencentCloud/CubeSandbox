#!/usr/bin/env python3
"""Render JuiceFS local and cross-node benchmark JSON as Markdown tables."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


CONCURRENCIES = (1, 5, 10)


def load_result(path: str) -> dict:
    try:
        result = json.loads(Path(path).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read result {path!r}: {exc}") from exc
    if result.get("suite") != "lifecycle" or not isinstance(result.get("cells"), dict):
        raise ValueError(f"{path!r} is not a lifecycle benchmark result")
    return result


def average(result: dict, section: str, concurrency: int, metric: str) -> float:
    cell = result["cells"].get(f"{section}_c{concurrency}")
    if not cell or not cell.get("summary", {}).get("valid"):
        raise ValueError(f"missing or invalid cell: {section}_c{concurrency}")
    value = cell["summary"].get("metrics", {}).get(metric, {}).get("avg")
    if not isinstance(value, (int, float)):
        raise ValueError(f"missing average for {section}_c{concurrency}.{metric}")
    return value


def table(result: dict, rows: list[tuple[str, str, str]]) -> list[str]:
    lines = ["| 指标 | 1 并发 | 5 并发 | 10 并发 |", "|---|---:|---:|---:|"]
    for label, section, metric in rows:
        values = [average(result, section, c, metric) for c in CONCURRENCIES]
        lines.append("| " + label + " | " + " | ".join(f"{v:.1f} ms" for v in values) + " |")
    return lines


def render(local: dict, cross: dict) -> str:
    cross_config = cross.get("config", {})
    origin, target = cross_config.get("origin", "origin"), cross_config.get("target", "target")
    sections = [
        ("4.1 模板启动与快照", local, [
            ("从模板启动", "template", "create_ms"),
            ("快照制作", "snapshot", "snapshot_ms"),
            ("快照数据就绪", "snapshot", "snapshot_share_ms"),
            ("快照制作到数据就绪总耗时", "snapshot", "snapshot_total_ready_ms"),
        ]),
        ("4.2 本机快照启动、暂停与恢复", local, [
            ("本机从快照启动", "fromsnap-local", "create_ms"),
            ("暂停", "pause-local", "pause_ms"),
            ("暂停包数据就绪", "pause-local", "pause_share_ms"),
            ("暂停到数据就绪总耗时", "pause-local", "pause_total_ready_ms"),
            ("本机恢复", "pause-local", "resume_ms"),
        ]),
        ("4.3 跨节点快照启动", cross, [
            (f"{origin}→{target} 首次启动", "fromsnap-cold", "create_ms"),
            (f"{origin}→{target} 预热启动", "fromsnap-warm", "create_ms"),
        ]),
        ("4.4 跨节点暂停恢复", cross, [
            (f"{origin} 暂停", "pause-cross", "pause_ms"),
            ("暂停包数据就绪", "pause-cross", "pause_share_ms"),
            ("暂停到数据就绪总耗时", "pause-cross", "pause_total_ready_ms"),
            (f"{target} 恢复", "pause-cross", "resume_ms"),
        ]),
    ]
    lines = ["## 四、测试结果", "", "表格仅展示平均耗时（Avg），所有数值均以 `ms` 为单位。"]
    for heading, result, rows in sections:
        lines.extend(["", f"### {heading}", "", *table(result, rows)])
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("local_json", help="cubebench_jfs_local.py output")
    parser.add_argument("cross_json", help="cubebench_jfs_cross_node.py output")
    parser.add_argument("--out", required=True, help="Markdown output path")
    args = parser.parse_args()
    try:
        output = render(load_result(args.local_json), load_result(args.cross_json))
    except ValueError as exc:
        parser.error(str(exc))
    Path(args.out).write_text(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
