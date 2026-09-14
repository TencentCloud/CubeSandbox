#!/usr/bin/env python3
"""一致性对照：把 cube-envd 的 fixture 与 Go 基线逐条对比，并生成可归档的结果。

用法：
  # 对照（CI 与本地同一条命令）
  python3 conformance.py diff --baseline fixtures/go-envd.json \
      --candidate fixtures/cube-envd.json --results RESULTS.md --strict

  # 用差异清单刷新 cube-envd/README.md 里的表格
  python3 conformance.py render-docs
  # 断言 README 表格与差异清单一致（CI 门禁）
  python3 conformance.py check-docs

判定口径：
  EQUAL              两侧完全一致
  DECLARED-DIFF      两侧不同，且已在该场景的差异清单里声明（见 declared_differences.toml）
  UNDECLARED-DIFF    两侧不同且未声明 → FAIL（这是本套件存在的意义）
  MISSING-*          一侧缺少记录 → FAIL
  DECLARED-BUT-EQUAL 清单里声明了差异，实际已一致 → 提示删除条目；--strict 下 FAIL
                     （防止"允许清单掩盖回归"：留着过期的条目等于给该场景永久放行）
"""

from __future__ import annotations

import argparse
import datetime
import json
import sys
from pathlib import Path
from typing import Any

try:
    import tomllib
except ModuleNotFoundError:  # Python < 3.11
    tomllib = None  # type: ignore[assignment]

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[3]
DECLARED_PATH = HERE / "declared_differences.toml"
README_PATH = REPO_ROOT / "cube-envd" / "README.md"
DOC_BEGIN = "<!-- cube-envd-declared-differences:begin -->"
DOC_END = "<!-- cube-envd-declared-differences:end -->"
MAX_DIFFS_PER_RECORD = 4


def load_json(path: Path) -> dict[str, Any]:
    """读取 fixture 文件。"""
    return json.loads(path.read_text())


def load_declared(path: Path) -> dict[str, list[dict[str, Any]]]:
    """读取差异清单（TOML）。"""
    if tomllib is None:
        raise SystemExit("python3.11+ is required for TOML parsing (declared_differences.toml)")
    data = tomllib.loads(path.read_text())
    return {
        "difference": data.get("difference", []),
        "aligned": data.get("aligned", []),
    }


def covered_records(declared: dict[str, Any], all_keys: list[str]) -> set[str]:
    """返回某条差异声明在本轮 fixture 里**确实存在**的记录键集合。

    与 `all_keys` 求交是刻意的：活体模式会跳过需要大载荷的场景（例如 limits_probe），
    此时清单条目"没有对应的记录"不等于"差异已追平"，不能报成 DECLARED-BUT-EQUAL。
    """
    wanted = (
        set(declared["records"])
        if declared.get("records")
        else {key for key in all_keys if key.startswith(f"{declared['scenario']}/")}
    )
    return wanted & set(all_keys)


def short(value: Any, limit: int = 160) -> str:
    """把取值压成一行摘要。"""
    text = json.dumps(value, ensure_ascii=False, sort_keys=True)
    return text if len(text) <= limit else text[: limit - 3] + "..."


def diff_values(left: Any, right: Any, path: str = "", found: list[str] | None = None) -> list[str]:
    """递归比较两个取值，返回形如 `path: left != right` 的差异描述。"""
    if found is None:
        found = []
    if len(found) >= MAX_DIFFS_PER_RECORD:
        return found
    if type(left) is not type(right):
        found.append(f"{path or '<root>'}: {short(left)} != {short(right)}")
        return found
    if isinstance(left, dict) and isinstance(right, dict):
        for key in sorted(set(left) | set(right)):
            if key not in left:
                found.append(f"{path}.{key}: <missing> != {short(right[key])}")
            elif key not in right:
                found.append(f"{path}.{key}: {short(left[key])} != <missing>")
            else:
                diff_values(left[key], right[key], f"{path}.{key}" if path else key, found)
            if len(found) >= MAX_DIFFS_PER_RECORD:
                break
        return found
    if isinstance(left, list) and isinstance(right, list):
        if len(left) != len(right):
            found.append(f"{path or '<root>'}: list length {len(left)} != {len(right)}")
            return found
        for index, (a, b) in enumerate(zip(left, right)):
            diff_values(a, b, f"{path}[{index}]", found)
            if len(found) >= MAX_DIFFS_PER_RECORD:
                break
        return found
    if left != right:
        found.append(f"{path or '<root>'}: {short(left)} != {short(right)}")
    return found


def compare(baseline: dict[str, Any], candidate: dict[str, Any], declared: dict[str, Any]) -> dict[str, Any]:
    """执行逐记录对比，返回判定结果。"""
    base_records: dict[str, Any] = baseline["records"]
    cand_records: dict[str, Any] = candidate["records"]
    all_keys = sorted(set(base_records) | set(cand_records))

    allowance: dict[str, dict[str, Any]] = {}
    for entry in declared["difference"]:
        for key in covered_records(entry, all_keys):
            allowance[key] = entry

    results: list[dict[str, Any]] = []
    for key in all_keys:
        if key not in base_records:
            verdict, details = ("MISSING-BASELINE", [f"baseline has no record {key}"])
        elif key not in cand_records:
            verdict, details = ("MISSING-CANDIDATE", [f"candidate has no record {key}"])
        else:
            details = diff_values(base_records[key], cand_records[key])
            verdict = "EQUAL" if not details else "DIFF"
        if verdict == "DIFF":
            verdict = "DECLARED-DIFF" if key in allowance else "UNDECLARED-DIFF"
        if verdict in {"MISSING-BASELINE", "MISSING-CANDIDATE"}:
            verdict = "DECLARED-DIFF" if key in allowance else verdict
        results.append({"key": key, "verdict": verdict, "details": details})

    # 清单里声明了、实际却没有差异的条目：提示删除（--strict 下失败）。
    stale: list[dict[str, Any]] = []
    for entry in declared["difference"]:
        keys = covered_records(entry, all_keys)
        observed = [
            item
            for item in results
            if item["key"] in keys and item["verdict"] == "DECLARED-DIFF"
        ]
        if not observed and keys:
            stale.append({"scenario": entry["scenario"], "records": sorted(keys)})

    uncaptured = sorted(
        {
            entry["scenario"]
            for entry in declared["difference"]
            if not covered_records(entry, all_keys)
        }
    )
    return {"results": results, "stale": stale, "uncaptured": uncaptured, "declared": declared}


def summarize(report: dict[str, Any]) -> dict[str, int]:
    """统计各判定的数量。"""
    counts: dict[str, int] = {}
    for item in report["results"]:
        counts[item["verdict"]] = counts.get(item["verdict"], 0) + 1
    counts["DECLARED-BUT-EQUAL"] = len(report["stale"])
    return counts


def failing(counts: dict[str, int], strict: bool) -> bool:
    """是否存在必须失败的情况。"""
    for verdict in ("UNDECLARED-DIFF", "MISSING-BASELINE", "MISSING-CANDIDATE"):
        if counts.get(verdict):
            return True
    return bool(strict and counts.get("DECLARED-BUT-EQUAL"))


def print_report(report: dict[str, Any], counts: dict[str, int], failing_run: bool) -> None:
    """把对比结果打印到 stderr（stdout 留给 RESULTS 文本）。"""
    for item in report["results"]:
        if item["verdict"] in {"EQUAL", "DECLARED-DIFF"}:
            continue
        print(f"{item['verdict']:<18} {item['key']}", file=sys.stderr)
        for detail in item["details"]:
            print(f"    {detail}", file=sys.stderr)
    for entry in report["stale"]:
        print(
            f"{'DECLARED-BUT-EQUAL':<18} {entry['scenario']} "
            f"({len(entry['records'])} record(s)) — 请从差异清单删除该条目",
            file=sys.stderr,
        )
    for entry in report["uncaptured"]:
        print(
            f"{'NOT-CAPTURED':<18} {entry} — 本轮 fixture 里没有该场景的记录"
            "（场景被跳过或两端都采集失败），既不算通过也不算回归",
            file=sys.stderr,
        )
    summary = " ".join(f"{name} {counts.get(name, 0)}" for name in sorted(counts))
    print(f"\n{summary}", file=sys.stderr)
    print("RESULT: FAIL" if failing_run else "RESULT: PASS", file=sys.stderr)


def render_results(report: dict[str, Any], baseline: dict[str, Any], candidate: dict[str, Any], counts: dict[str, int]) -> str:
    """生成可归档的 RESULTS.md（由脚本生成，不手写）。"""
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")
    lines = [
        "# cube-envd 一致性对照结果（由 conformance.py 生成，请勿手改）",
        "",
        f"- 生成时间：{stamp}",
        f"- 基线：`{baseline['implementation']}`（{baseline['endpoint']}，采集于 {baseline['captured_at']}）",
        f"- 候选：`{candidate['implementation']}`（{candidate['endpoint']}，采集于 {candidate['captured_at']}）",
        f"- 计数：{' / '.join(f'{name}={counts.get(name, 0)}' for name in sorted(counts))}",
        "",
        "## 判定说明",
        "",
        "| 判定 | 含义 |",
        "|---|---|",
        "| EQUAL | 两侧逐字段一致 |",
        "| DECLARED-DIFF | 差异已在 `declared_differences.toml` 声明 |",
        "| UNDECLARED-DIFF | 未声明的差异（失败） |",
        "| MISSING-BASELINE / MISSING-CANDIDATE | 一侧缺少记录（失败） |",
        "| DECLARED-BUT-EQUAL | 清单过期（`--strict` 下失败） |",
        "",
        "## 已声明的差异",
        "",
        "| 场景 | 面 | 基线 | cube-envd |",
        "|---|---|---|---|",
    ]
    for entry in report["declared"]["difference"]:
        lines.append(
            f"| `{entry['scenario']}` | {entry.get('surface', '')} | {entry.get('baseline', '')} | {entry.get('ours', '')} |"
        )
    lines += ["", "## 未声明差异（必须为 0）", ""]
    undeclared = [item for item in report["results"] if item["verdict"] == "UNDECLARED-DIFF"]
    if undeclared:
        for item in undeclared:
            lines.append(f"- `{item['key']}`")
            for detail in item["details"]:
                lines.append(f"  - {detail}")
    else:
        lines.append("无。")
    lines += ["", "## 逐记录判定", "", "| 记录 | 判定 |", "|---|---|"]
    for item in report["results"]:
        lines.append(f"| `{item['key']}` | {item['verdict']} |")
    lines.append("")
    return "\n".join(lines)


def render_docs_table(declared: dict[str, Any]) -> str:
    """生成 README 里的差异表格（含对齐项）。"""
    lines = [
        DOC_BEGIN,
        "<!-- 由 tests/e2e/cube_envd/conformance/conformance.py render-docs 生成；请勿手改。 -->",
        "",
        "| 面 | Go envd 0.5.13 基线 | cube-envd | 依据 |",
        "|---|---|---|---|",
    ]
    for entry in declared["difference"]:
        reason = " ".join(entry.get("reason", "").split())
        lines.append(
            f"| {entry.get('surface', entry['scenario'])} | {entry.get('baseline', '')} | "
            f"{entry.get('ours', '')} | {reason} |"
        )
    lines += ["", "已决定与基线保持一致的行为（不是差异，但同样是被评审过的取值）：", "",
              "| 面 | 基线 | cube-envd | 说明 |", "|---|---|---|---|"]
    for entry in declared["aligned"]:
        reason = " ".join(entry.get("reason", "").split())
        lines.append(
            f"| {entry.get('surface', '')} | {entry.get('baseline', '')} | "
            f"{entry.get('ours', '')} | {reason} |"
        )
    lines += [
        "",
        "完整清单（含机器可读的允许范围）在",
        "[`tests/e2e/cube_envd/conformance/declared_differences.toml`](../tests/e2e/cube_envd/conformance/declared_differences.toml)；",
        "对照套件的实测结果由 `conformance.py … --results RESULTS.md` 生成到同目录的",
        "`RESULTS.md`（**生成物，不入库**；CI 把它作为 `cube-envd-conformance-results`",
        "artifact 上传）。",
        DOC_END,
    ]
    return "\n".join(lines)


def render_docs(declared: dict[str, Any], *, check: bool) -> int:
    """写入或校验 README 中的差异表格。"""
    if not README_PATH.exists():
        print(f"README not found: {README_PATH}", file=sys.stderr)
        return 2
    text = README_PATH.read_text()
    block = render_docs_table(declared)
    begin = text.find(DOC_BEGIN)
    end = text.find(DOC_END)
    if begin == -1 or end == -1:
        if check:
            print(
                f"{README_PATH} 缺少 {DOC_BEGIN} / {DOC_END} 标记块；先运行 render-docs",
                file=sys.stderr,
            )
            return 1
        text = text.rstrip() + "\n\n## Declared differences\n\n" + block + "\n"
    else:
        text = text[:begin] + block + text[end + len(DOC_END) :]
    if check:
        current = README_PATH.read_text()
        if current != text:
            print(
                "cube-envd/README.md 的差异表格与 declared_differences.toml 不一致；"
                "运行 conformance.py render-docs 后提交",
                file=sys.stderr,
            )
            return 1
        print("README declared-differences table is in sync", file=sys.stderr)
        return 0
    README_PATH.write_text(text)
    print(f"updated {README_PATH}", file=sys.stderr)
    return 0


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """解析命令行参数。"""
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command")
    diff = sub.add_parser("diff", help="对比两个 fixture")
    diff.add_argument("--baseline", required=True)
    diff.add_argument("--candidate", required=True)
    diff.add_argument("--declared", default=str(DECLARED_PATH))
    diff.add_argument("--results", default=None, help="把生成的结果写到该路径")
    diff.add_argument("--strict", action="store_true", help="过期清单条目也判失败")
    sub.add_parser("render-docs", help="用差异清单刷新 README 表格")
    sub.add_parser("check-docs", help="断言 README 表格与差异清单一致")
    args = parser.parse_args(argv)
    if args.command is None:
        args.command = "diff"
        args.baseline = None
    return args


def main(argv: list[str] | None = None) -> int:
    """入口。"""
    args = parse_args(argv)

    if args.command in {"render-docs", "check-docs"}:
        declared = load_declared(Path(getattr(args, "declared", DECLARED_PATH)))
        return render_docs(declared, check=args.command == "check-docs")

    if not args.baseline or not args.candidate:
        print("diff 需要 --baseline 与 --candidate；见 --help", file=sys.stderr)
        return 2

    declared = load_declared(Path(args.declared))
    baseline = load_json(Path(args.baseline))
    candidate = load_json(Path(args.candidate))
    report = compare(baseline, candidate, declared)
    counts = summarize(report)
    failed = failing(counts, args.strict)
    print_report(report, counts, failed)

    if args.results:
        Path(args.results).write_text(render_results(report, baseline, candidate, counts))
        print(f"wrote {args.results}", file=sys.stderr)

    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
