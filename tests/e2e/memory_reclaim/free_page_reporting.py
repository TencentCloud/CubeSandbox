#!/usr/bin/env python3
"""Node-local free-page-reporting E2E for Cube sandboxes."""

from __future__ import annotations

import argparse
import json
import os
import platform
import shlex
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Callable


def add_repo_sdk_to_path() -> None:
    configured = os.environ.get("CUBE_REPO_ROOT")
    candidates = [Path(configured).expanduser().resolve()] if configured else []
    candidates.extend(Path(__file__).resolve().parents)
    for root in candidates:
        sdk = root / "sdk" / "python"
        if (sdk / "cubesandbox").is_dir():
            sys.path.insert(0, str(sdk))
            return
    raise RuntimeError("cannot locate repository sdk/python")


add_repo_sdk_to_path()

from cubesandbox import (  # noqa: E402
    ApiError,
    Config,
    Sandbox,
    SandboxNotFoundError,
    Template,
    TemplateBuild,
    TemplateNotFoundError,
)


DEFAULT_RUNTIME_STATE_ROOTS = (
    Path("/data/cubelet/state/io.containerd.runtime.v2.task/default"),
    Path("/data/cubelet/root/io.containerd.runtime.v2.task/default"),
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--image", default=os.environ.get("CUBE_E2E_IMAGE"))
    parser.add_argument("--node", default=os.environ.get("CUBE_E2E_NODE"))
    parser.add_argument("--template-build-timeout", type=float, default=900.0)
    parser.add_argument("--load-memory-mib", type=int, default=1536)
    parser.add_argument("--baseline-limit-mib", type=int, default=512)
    parser.add_argument("--peak-delta-mib", type=int, default=1024)
    parser.add_argument("--release-slack-mib", type=int, default=384)
    parser.add_argument("--poll-timeout", type=float, default=90.0)
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--runtime-state-root",
        action="append",
        type=Path,
        help="containerd task-state root; may be specified more than once",
    )
    parser.add_argument("--keep-resources", action="store_true")
    args = parser.parse_args()
    if bool(args.template) == bool(args.image):
        parser.error("set exactly one of --template/CUBE_TEMPLATE_ID or --image/CUBE_E2E_IMAGE")
    if not args.node:
        parser.error("set --node or CUBE_E2E_NODE to pin the sandbox to this node")
    for name in (
        "load_memory_mib",
        "baseline_limit_mib",
        "peak_delta_mib",
        "release_slack_mib",
    ):
        if getattr(args, name) <= 0:
            parser.error(f"--{name.replace('_', '-')} must be positive")
    return args


def log(message: str) -> None:
    print(f"[free-page-reporting-e2e] {message}", file=sys.stderr, flush=True)


class FatalProbeError(RuntimeError):
    pass


def wait_for(
    description: str,
    timeout: float,
    probe: Callable[[], object | None],
    *,
    interval: float = 1.0,
) -> object:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = probe()
            if value is not None:
                return value
        except FatalProbeError:
            raise
        except Exception as exc:  # noqa: BLE001 - report the last transient probe error
            last_error = exc
        time.sleep(interval)
    suffix = f": {last_error}" if last_error else ""
    raise TimeoutError(f"timed out waiting for {description}{suffix}")


def process_rss_kib(pid: int) -> int:
    for line in Path(f"/proc/{pid}/status").read_text(encoding="utf-8").splitlines():
        if line.startswith("VmRSS:"):
            return int(line.split()[1])
    raise RuntimeError(f"VmRSS is missing for pid {pid}")


def free_page_reporting_enabled(value: bytes | None, machine: str) -> bool:
    architecture_default = machine.lower() not in {"aarch64", "arm64"}
    if value is None:
        return architecture_default
    normalized = value.strip().lower()
    if normalized in {b"1", b"on", b"true", b"yes"}:
        return True
    if normalized in {b"0", b"off", b"false", b"no"}:
        return False
    return architecture_default


def require_free_page_reporting(pid: int) -> None:
    try:
        environ = (Path(f"/proc/{pid}") / "environ").read_bytes().split(b"\0")
    except (FileNotFoundError, PermissionError) as exc:
        raise RuntimeError(
            f"cannot inspect shim environment for pid {pid}: {exc}"
        ) from exc
    value = next(
        (
            entry.partition(b"=")[2]
            for entry in environ
            if entry.startswith(b"CUBE_BALLOON_FREE_PAGE_REPORTING=")
        ),
        None,
    )
    expected = os.environ.get("CUBE_BALLOON_FREE_PAGE_REPORTING")
    if expected and value is None:
        raise RuntimeError(
            "CUBE_BALLOON_FREE_PAGE_REPORTING is set for the E2E runner but "
            "missing from the shim environment; restart Cubelet and verify its "
            "environment propagation"
        )
    if not free_page_reporting_enabled(value, platform.machine()):
        raise RuntimeError(
            "free-page reporting is disabled in the sandbox shim; set "
            "CUBE_BALLOON_FREE_PAGE_REPORTING=on, restart Cubelet, and create "
            "or rebuild the template before running this E2E"
        )


def is_sandbox_shim_pid(pid: int, sandbox_id: str) -> bool:
    try:
        proc_dir = Path(f"/proc/{pid}")
        args = [
            value.decode(errors="replace")
            for value in (proc_dir / "cmdline").read_bytes().split(b"\0")
            if value
        ]
        if not args or Path(args[0]).name != "containerd-shim-cube-rs":
            return False
        if not any(
            arg == "-id" and args[index + 1] == sandbox_id
            for index, arg in enumerate(args[:-1])
        ):
            return False
        os.kill(pid, 0)
        return True
    except (FileNotFoundError, IndexError, PermissionError, ProcessLookupError):
        return False


def read_live_pid(path: Path, sandbox_id: str) -> int | None:
    try:
        pid = int(path.read_text(encoding="utf-8").strip())
    except (FileNotFoundError, PermissionError, ValueError):
        return None
    return pid if is_sandbox_shim_pid(pid, sandbox_id) else None


def find_shim_pid(sandbox_id: str, excluded_pid: int | None = None) -> int | None:
    for proc_dir in Path("/proc").iterdir():
        if not proc_dir.name.isdigit():
            continue
        pid = int(proc_dir.name)
        if pid == excluded_pid:
            continue
        if is_sandbox_shim_pid(pid, sandbox_id):
            return pid
    return None


def wait_for_vmm_pid(
    roots: tuple[Path, ...],
    sandbox_id: str,
    timeout: float,
    *,
    previous_pid: int | None = None,
) -> int:
    candidates = [root / sandbox_id / "vmm.pid" for root in roots]

    def probe() -> int | None:
        for candidate in candidates:
            pid = read_live_pid(candidate, sandbox_id)
            if pid is not None and pid != previous_pid:
                return pid
        pid = find_shim_pid(sandbox_id, previous_pid)
        if pid is not None:
            return pid
        return None

    value = wait_for("node-local VMM pid", timeout, probe)
    assert isinstance(value, int)
    return value


def run_guest_command(sandbox: Sandbox, command: str, timeout: float) -> None:
    result = sandbox.commands.run(command, timeout=timeout)
    if result.exit_code != 0:
        raise RuntimeError(
            f"guest command failed with exit code {result.exit_code}: {result.stderr}"
        )


def wait_for_guest_marker(sandbox: Sandbox, path: str, timeout: float) -> None:
    command = f"test -e {shlex.quote(path)}"

    def probe() -> bool | None:
        result = sandbox.commands.run(command, timeout=min(timeout, 15.0))
        if result.exit_code == 0:
            return True
        if result.exit_code == 1:
            return None
        raise FatalProbeError(
            f"guest marker probe failed with exit code {result.exit_code}: {result.stderr}"
        )

    wait_for("guest workload readiness", timeout, probe)


def wait_for_data_plane(sandbox_id: str, config: Config, timeout: float) -> Sandbox:
    retryable_statuses = {502, 503, 504}

    def probe() -> Sandbox | None:
        candidate = Sandbox.connect(sandbox_id, config=config)
        try:
            run_guest_command(candidate, "true", 15)
        except ApiError as exc:
            if exc.status_code in retryable_statuses:
                return None
            raise FatalProbeError(f"non-retryable data-plane error: {exc}") from exc
        return candidate

    value = wait_for("CubeProxy/envd readiness", timeout, probe, interval=2.0)
    assert isinstance(value, Sandbox)
    return value


def start_template_build(args: argparse.Namespace, config: Config) -> TemplateBuild:
    name = f"free-page-reporting-e2e-{int(time.time())}"
    log(f"building temporary 2 GiB template {name}")
    return Template.build(
        name=name,
        image=args.image,
        cpu_count=500,
        memory_mb=2048,
        writable_layer_size="1G",
        network_type="tap",
        nodes=[args.node],
        config=config,
    )


def wait_for_template_build(
    template_id: str,
    build_id: str,
    args: argparse.Namespace,
    config: Config,
) -> str:

    terminal_failure = {"ERROR", "FAILED", "CANCELED", "CANCELLED"}

    def probe() -> str | None:
        current = Template.get_build_status(
            template_id,
            build_id,
            config=config,
        )
        status = current.status.upper()
        if status in terminal_failure:
            raise FatalProbeError(
                f"template build failed: status={current.status} "
                f"phase={current.phase} error={current.error_message or current.message}"
            )
        if status in {"READY", "SUCCESS", "SUCCEEDED"}:
            return template_id
        return None

    value = wait_for(
        "temporary template build",
        args.template_build_timeout,
        probe,
        interval=3.0,
    )
    assert isinstance(value, str)
    return value


def cleanup_sandbox(
    sandbox: Sandbox,
    roots: tuple[Path, ...],
    timeout: float,
) -> None:
    sandbox_id = sandbox.sandbox_id
    try:
        sandbox.kill()
    except SandboxNotFoundError:
        pass

    def probe() -> bool | None:
        try:
            sandbox.get_info()
            return None
        except SandboxNotFoundError:
            pass
        if find_shim_pid(sandbox_id) is not None:
            return None
        if any((root / sandbox_id).exists() for root in roots):
            return None
        return True

    wait_for("sandbox API, process, and task-state cleanup", timeout, probe)


def cleanup_template(template_id: str, config: Config, timeout: float) -> None:
    try:
        Template.delete(template_id, config=config)
    except TemplateNotFoundError:
        return

    def probe() -> bool | None:
        try:
            Template.get(template_id, config=config)
        except TemplateNotFoundError:
            return True
        return None

    wait_for("temporary template cleanup", timeout, probe)


def run_reclaim_cycle(
    sandbox: Sandbox,
    vmm_pid: int,
    phase: str,
    args: argparse.Namespace,
) -> dict[str, float]:
    mib_to_kib = 1024
    baseline = wait_for(
        f"{phase} RSS baseline",
        args.poll_timeout,
        lambda: (
            rss
            if (rss := process_rss_kib(vmm_pid))
            <= args.baseline_limit_mib * mib_to_kib
            else None
        ),
    )
    assert isinstance(baseline, int)

    marker = f"/tmp/free-page-reporting-{phase.replace(' ', '-')}"
    ready = f"{marker}.ready"
    release = f"{marker}.release"
    run_guest_command(sandbox, f"rm -f {shlex.quote(ready)} {shlex.quote(release)}", 15)
    code = (
        "import gc,os,time\n"
        f"buf=bytearray({args.load_memory_mib}*1024*1024)\n"
        "for offset in range(0,len(buf),4096): buf[offset]=1\n"
        f"open({ready!r},'w').close()\n"
        f"while not os.path.exists({release!r}): time.sleep(0.1)\n"
        "del buf\n"
        "gc.collect()\n"
    )

    with ThreadPoolExecutor(max_workers=1) as pool:
        workload = pool.submit(
            run_guest_command,
            sandbox,
            f"python3 -c {shlex.quote(code)}",
            args.poll_timeout * 3,
        )
        try:
            wait_for_guest_marker(sandbox, ready, args.poll_timeout)
            peak = process_rss_kib(vmm_pid)
            minimum_peak = baseline + args.peak_delta_mib * mib_to_kib
            if peak < minimum_peak:
                raise RuntimeError(
                    f"{phase} RSS rose by only {(peak - baseline) / mib_to_kib:.1f} MiB; "
                    f"expected at least {args.peak_delta_mib} MiB"
                )
        finally:
            run_guest_command(sandbox, f"touch {shlex.quote(release)}", 15)
        workload.result()

    released = wait_for(
        f"{phase} RSS reclamation",
        args.poll_timeout,
        lambda: (
            rss
            if (rss := process_rss_kib(vmm_pid))
            <= baseline + args.release_slack_mib * mib_to_kib
            else None
        ),
    )
    assert isinstance(released, int)
    run_guest_command(sandbox, f"rm -f {shlex.quote(ready)} {shlex.quote(release)}", 15)

    sample = {
        "baseline_rss_mib": round(baseline / 1024, 1),
        "peak_rss_mib": round(peak / 1024, 1),
        "released_rss_mib": round(released / 1024, 1),
        "reclaimed_rss_mib": round((peak - released) / 1024, 1),
    }
    log(f"{phase}: {sample}")
    return sample


def emit_result(result: dict[str, object], output: Path | None) -> None:
    encoded = json.dumps(result, indent=2, sort_keys=True)
    print(encoded)
    if output:
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(encoded + "\n", encoding="utf-8")


def main() -> int:
    args = parse_args()
    roots = tuple(args.runtime_state_root or DEFAULT_RUNTIME_STATE_ROOTS)
    config = Config()
    sandbox: Sandbox | None = None
    built_template_id: str | None = None
    result: dict[str, object] = {
        "node": args.node,
        "load_memory_mib": args.load_memory_mib,
        "samples": {},
        "status": "FAIL",
    }
    try:
        template_id = args.template
        if args.image:
            build = start_template_build(args, config)
            if not build.template_id:
                raise RuntimeError(f"template build response is missing template ID: {build}")
            built_template_id = str(build.template_id)
            if not build.build_id:
                raise RuntimeError(f"template build response is missing build ID: {build}")
            template_id = wait_for_template_build(
                built_template_id,
                str(build.build_id),
                args,
                config,
            )
        result["template_id"] = template_id

        log("creating a node-pinned sandbox through CubeAPI")
        sandbox = Sandbox.create(
            template=template_id,
            timeout=900,
            distribution_scope=[args.node],
            config=config,
        )
        sandbox_id = sandbox.sandbox_id
        result["sandbox_id"] = sandbox_id
        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)
        cold_pid = wait_for_vmm_pid(roots, sandbox_id, args.poll_timeout)
        require_free_page_reporting(cold_pid)
        result["samples"]["cold_boot"] = run_reclaim_cycle(
            sandbox, cold_pid, "cold boot", args
        )

        log("pausing and reconnecting the sandbox through CubeAPI")
        sandbox.pause(timeout=args.poll_timeout)
        sandbox = Sandbox.connect(sandbox_id, config=config)
        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)
        restored_pid = wait_for_vmm_pid(
            roots,
            sandbox_id,
            args.poll_timeout,
            previous_pid=cold_pid,
        )
        require_free_page_reporting(restored_pid)
        result["samples"]["pause_resume"] = run_reclaim_cycle(
            sandbox, restored_pid, "pause resume", args
        )
        result["status"] = "PASS"
        return 0
    finally:
        active_failure = sys.exc_info()[0] is not None
        cleanup_errors: list[str] = []
        if sandbox is not None and not args.keep_resources:
            try:
                cleanup_sandbox(sandbox, roots, args.poll_timeout)
                result["cleanup"] = "complete"
            except Exception as exc:  # noqa: BLE001 - preserve the original failure
                cleanup_errors.append(f"sandbox: {exc}")
        elif sandbox is not None:
            result["cleanup"] = "kept"
        if built_template_id and not args.keep_resources:
            try:
                cleanup_template(built_template_id, config, args.poll_timeout)
                result["template_cleanup"] = "complete"
            except Exception as exc:  # noqa: BLE001 - preserve the original failure
                cleanup_errors.append(f"template: {exc}")
        if cleanup_errors:
            result["cleanup_errors"] = cleanup_errors
            if not active_failure:
                result["status"] = "FAIL"
        emit_result(result, args.output)
        if cleanup_errors and not active_failure:
            raise RuntimeError("; ".join(cleanup_errors))


if __name__ == "__main__":
    raise SystemExit(main())
