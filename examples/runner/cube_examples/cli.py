"""CLI entrypoint for the CubeSandbox examples runner.

Usage examples::

    # List all discovered examples and their tags
    python -m cube_examples list

    # Run everything against a deployment
    python -m cube_examples run --api-url http://127.0.0.1:3000

    # Run only smoke-tagged examples, supplying template ids
    python -m cube_examples run --tags smoke \\
        --template code=tpl-abc123 --template browser=tpl-def456

    # Run a single example, verbose, write reports
    python -m cube_examples run --only code-sandbox-quickstart -v \\
        --report-json out/report.json --report-md out/report.md
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .manifest import Manifest, ManifestError, discover
from .report import build_summary, write_json, write_markdown
from .runner import ExampleResult, run_example

_STATUS_EMOJI = {"passed": "✅", "failed": "❌", "skipped": "⏭️", "error": "💥"}


def _default_examples_root() -> Path:
    # runner/cube_examples/cli.py -> examples/
    return Path(__file__).resolve().parents[2]


def _filter(
    manifests: list[Manifest],
    tags: list[str] | None,
    only: list[str] | None,
) -> list[Manifest]:
    out = manifests
    if only:
        only_set = set(only)
        out = [m for m in out if m.name in only_set]
    if tags:
        tag_set = set(tags)
        out = [m for m in out if tag_set & set(m.tags)]
    return out


def _build_base_env(args: argparse.Namespace) -> dict[str, str]:
    env: dict[str, str] = {}
    if args.api_url:
        # E2B SDK reads E2B_API_URL; CubeSDK reads CUBE_API_URL. Set both.
        env["E2B_API_URL"] = args.api_url
        env["CUBE_API_URL"] = args.api_url
    if args.api_key:
        env["E2B_API_KEY"] = args.api_key
    # Template aliases: --template code=tpl-xxx -> CUBE_TEMPLATE_ID resolution
    # happens per-example based on requires_template; we stash the map in env
    # using a namespaced prefix the manifest env can reference if needed.
    for pair in args.template or []:
        if "=" not in pair:
            raise SystemExit(f"--template expects alias=id, got: {pair}")
        alias, tid = pair.split("=", 1)
        env[f"CUBE_TEMPLATE_{alias.upper()}"] = tid
    if args.ssl_cert_file:
        env["SSL_CERT_FILE"] = args.ssl_cert_file
        env["NODE_EXTRA_CA_CERTS"] = args.ssl_cert_file
    return env


def _resolve_template(manifest: Manifest, base_env: dict[str, str]) -> dict[str, str]:
    """Resolve CUBE_TEMPLATE_ID for an example from its requires_template alias."""
    if not manifest.requires_template:
        return {}
    alias = manifest.requires_template.upper()
    key = f"CUBE_TEMPLATE_{alias}"
    if key in base_env:
        return {"CUBE_TEMPLATE_ID": base_env[key]}
    return {}


def cmd_list(args: argparse.Namespace) -> int:
    manifests = discover(args.examples_root)
    manifests = _filter(manifests, args.tags, args.only)
    if not manifests:
        print("No examples matched.")
        return 0
    print(f"Discovered {len(manifests)} example(s):\n")
    for m in manifests:
        tags = ", ".join(m.tags) or "-"
        tpl = m.requires_template or "-"
        flag = " [skip]" if m.skip else ""
        print(f"  {m.name:32s} tags={tags:20s} template={tpl}{flag}")
        if m.description:
            print(f"      {m.description}")
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    manifests = discover(args.examples_root)
    manifests = _filter(manifests, args.tags, args.only)
    if not manifests:
        print("No examples matched the filters.", file=sys.stderr)
        return 2

    base_env = _build_base_env(args)
    results: list[ExampleResult] = []

    print(f"Running {len(manifests)} example(s)...\n")
    for m in manifests:
        print(f"==> {m.name}")
        per_example_env = {**base_env, **_resolve_template(m, base_env)}
        # Guard: example needs a template but none was provided.
        if m.requires_template and "CUBE_TEMPLATE_ID" not in per_example_env and not m.skip:
            res = ExampleResult(
                name=m.name,
                path=str(m.path),
                tags=m.tags,
                status="skipped",
                duration_s=0.0,
                message=(
                    f"no template id for alias '{m.requires_template}' "
                    f"(pass --template {m.requires_template}=<id>)"
                ),
            )
        else:
            res = run_example(
                m, per_example_env, verbose=args.verbose, run_setup=not args.no_setup
            )
        emoji = _STATUS_EMOJI.get(res.status, "?")
        print(f"    {emoji} {res.status} ({res.duration_s}s)")
        if res.message:
            print(f"    {res.message}")
        results.append(res)

    summary = build_summary(results, meta=_meta(args))
    t = summary["totals"]
    print(
        f"\nSummary: {t['passed']} passed, {t['failed']} failed, "
        f"{t['error']} error, {t['skipped']} skipped"
    )

    if args.report_json:
        write_json(results, Path(args.report_json), meta=_meta(args))
        print(f"JSON report:     {args.report_json}")
    if args.report_md:
        write_markdown(results, Path(args.report_md), meta=_meta(args))
        print(f"Markdown report: {args.report_md}")

    return 0 if summary["ok"] else 1


def _meta(args: argparse.Namespace) -> dict:
    meta = {}
    if args.api_url:
        meta["api_url"] = args.api_url
    if args.tags:
        meta["tags"] = ",".join(args.tags)
    if args.label:
        meta["label"] = args.label
    return meta


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="cube-examples",
        description="Discover, run, and assert CubeSandbox example scripts.",
    )
    p.add_argument(
        "--examples-root",
        type=Path,
        default=_default_examples_root(),
        help="Root directory to scan for cube-example.yaml (default: examples/).",
    )
    sub = p.add_subparsers(dest="command", required=True)

    pl = sub.add_parser("list", help="List discovered examples.")
    pl.add_argument("--tags", nargs="*", help="Filter by tags (any match).")
    pl.add_argument("--only", nargs="*", help="Filter by example name(s).")
    pl.set_defaults(func=cmd_list)

    pr = sub.add_parser("run", help="Run examples and assert outcomes.")
    pr.add_argument("--api-url", help="CubeAPI base URL, e.g. http://127.0.0.1:3000")
    pr.add_argument("--api-key", default="e2b_000000", help="E2B_API_KEY (default: e2b_000000).")
    pr.add_argument(
        "--template",
        action="append",
        metavar="alias=id",
        help="Map a template alias to an id, e.g. --template code=tpl-abc. Repeatable.",
    )
    pr.add_argument("--ssl-cert-file", help="Path to mkcert rootCA.pem for TLS examples.")
    pr.add_argument("--tags", nargs="*", help="Filter by tags (any match).")
    pr.add_argument("--only", nargs="*", help="Filter by example name(s).")
    pr.add_argument("--no-setup", action="store_true", help="Skip setup commands (pip install).")
    pr.add_argument("-v", "--verbose", action="store_true", help="Print each command.")
    pr.add_argument("--report-json", help="Write JSON report to this path.")
    pr.add_argument("--report-md", help="Write Markdown report to this path.")
    pr.add_argument("--label", help="Free-form label recorded in report meta (e.g. version).")
    pr.set_defaults(func=cmd_run)

    return p


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except ManifestError as exc:
        print(f"Manifest error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
