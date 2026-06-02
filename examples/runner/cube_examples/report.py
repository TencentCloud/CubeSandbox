"""Report generation: JSON (machine) and Markdown (human / release notes)."""

from __future__ import annotations

import dataclasses
import datetime as _dt
import json
from pathlib import Path

from .runner import ExampleResult

_STATUS_EMOJI = {
    "passed": "✅",
    "failed": "❌",
    "skipped": "⏭️",
    "error": "💥",
}


def build_summary(results: list[ExampleResult], meta: dict | None = None) -> dict:
    counts = {"passed": 0, "failed": 0, "skipped": 0, "error": 0}
    for r in results:
        counts[r.status] = counts.get(r.status, 0) + 1
    return {
        "generated_at": _dt.datetime.now(_dt.timezone.utc).isoformat(),
        "meta": meta or {},
        "totals": {
            "examples": len(results),
            **counts,
        },
        "ok": counts["failed"] == 0 and counts["error"] == 0,
        "results": [dataclasses.asdict(r) for r in results],
    }


def write_json(results: list[ExampleResult], out_path: Path, meta: dict | None = None) -> None:
    summary = build_summary(results, meta)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")


def write_markdown(results: list[ExampleResult], out_path: Path, meta: dict | None = None) -> None:
    summary = build_summary(results, meta)
    lines: list[str] = []
    lines.append("# CubeSandbox Examples Verification Report")
    lines.append("")
    lines.append(f"- Generated: `{summary['generated_at']}`")
    for k, v in (meta or {}).items():
        lines.append(f"- {k}: `{v}`")
    t = summary["totals"]
    overall = "PASS ✅" if summary["ok"] else "FAIL ❌"
    lines.append(f"- Overall: **{overall}**")
    lines.append(
        f"- Totals: {t['examples']} examples — "
        f"{t['passed']} passed, {t['failed']} failed, "
        f"{t['error']} error, {t['skipped']} skipped"
    )
    lines.append("")
    lines.append("## Results")
    lines.append("")
    lines.append("| Example | Status | Duration | Tags | Notes |")
    lines.append("|---------|--------|----------|------|-------|")
    for r in results:
        emoji = _STATUS_EMOJI.get(r.status, "?")
        tags = ", ".join(r.tags)
        note = r.message
        if not note:
            failed_steps = [s.name for s in r.steps if not s.passed]
            if failed_steps:
                note = "failed steps: " + ", ".join(failed_steps)
        lines.append(
            f"| `{r.name}` | {emoji} {r.status} | {r.duration_s}s | {tags} | {note} |"
        )
    lines.append("")

    # Failure detail.
    failures = [r for r in results if r.status in ("failed", "error")]
    if failures:
        lines.append("## Failure Details")
        lines.append("")
        for r in failures:
            lines.append(f"### {_STATUS_EMOJI.get(r.status, '?')} `{r.name}`")
            lines.append("")
            if r.message:
                lines.append(f"> {r.message}")
                lines.append("")
            for s in r.steps:
                if s.passed:
                    continue
                lines.append(f"- **{s.name}** (`{s.command}`, exit={s.exit_code})")
                for f in s.failures:
                    lines.append(f"  - {f}")
                if s.stderr_tail.strip():
                    lines.append("  - stderr tail:")
                    lines.append("    ```")
                    for ln in s.stderr_tail.strip().splitlines()[-15:]:
                        lines.append(f"    {ln}")
                    lines.append("    ```")
            lines.append("")

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
