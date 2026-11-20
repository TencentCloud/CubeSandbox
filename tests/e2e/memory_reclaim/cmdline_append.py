#!/usr/bin/env python3
"""Node-local guest kernel cmdline append E2E for Cube sandboxes.

Builds a template whose first container carries
``cube.vm.kernel.cmdline.append`` (DAMON params, no explicit monitor region),
creates a sandbox from it, then asserts that the booted guest's
``/proc/cmdline`` contains the injected params plus the auto-filled DAMON
monitor region.

Video params are out of scope here: the E2B facade (``NewSandbox``) has no
``annotations`` field, so ``cloud.tencent.com/video/*`` cannot be set through
the SDK. The video+DAMON merge is covered by the Cubelet spec-level unit test
``TestGenDamonMonitorRegionOptMergesVideoAndDamon``.

The template is created through CubeMaster's internal API because the E2B
facade (CubeAPI ``/templates`` and ``/sandboxes``) deliberately drops container
annotations: ``CreateTemplateContainerOverrides`` has no annotations field and
``create_sandbox`` hardcodes ``containers = []``. The sandbox itself is still
created through the SDK so it exercises the inherited-annotation path.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import requests  # noqa: E402

import free_page_reporting as harness  # noqa: E402
from cubesandbox import Sandbox  # noqa: E402

wait_for = harness.wait_for
cleanup_sandbox = harness.cleanup_sandbox
cleanup_template = harness.cleanup_template
wait_for_data_plane = harness.wait_for_data_plane
emit_result = harness.emit_result
DEFAULT_RUNTIME_STATE_ROOTS = harness.DEFAULT_RUNTIME_STATE_ROOTS


def log(message: str) -> None:
    print(f"[cmdline-append-e2e] {message}", file=sys.stderr, flush=True)

DEFAULT_MASTER_URL = "http://127.0.0.1:8089"
CMDLINE_ANNOTATION = "cube.vm.kernel.cmdline.append"
DAMON_PARAMS = (
    "damon_reclaim.enabled=Y",
    "damon_reclaim.min_age=1000000",
    "page_reporting.page_reporting_order=6",
)
# The DAMON guest kernel exposes the sysfs admin interface; the default guest
# kernel ships without CONFIG_DAMON_RECLAIM, so the interface is absent.
KERNEL_PROBES = {
    "none": None,
    "damon": "test -e /sys/kernel/mm/damon/admin",
    "default": "test ! -e /sys/kernel/mm/damon/admin",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--image",
        default=os.environ.get("CUBE_E2E_IMAGE"),
        help="OCI image used to build the temporary annotated template",
    )
    parser.add_argument("--node", default=os.environ.get("CUBE_E2E_NODE"))
    parser.add_argument(
        "--master-url",
        default=os.environ.get("CUBE_MASTER_URL", DEFAULT_MASTER_URL),
        help="CubeMaster internal API base URL (annotation ingress)",
    )
    parser.add_argument(
        "--expected-memory-mib",
        type=int,
        default=2048,
        help="guest RAM the temporary template (and inherited sandbox) requests",
    )
    parser.add_argument("--template-build-timeout", type=float, default=900.0)
    parser.add_argument("--poll-timeout", type=float, default=120.0)
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--runtime-state-root",
        action="append",
        type=Path,
        help="containerd task-state root; may be specified more than once",
    )
    parser.add_argument("--keep-resources", action="store_true")
    parser.add_argument(
        "--expect-kernel",
        choices=tuple(KERNEL_PROBES),
        default="none",
        help="also assert the booted guest kernel build",
    )
    args = parser.parse_args()
    if not args.image:
        parser.error("set --image or CUBE_E2E_IMAGE to build an annotated template")
    if not args.node:
        parser.error("set --node or CUBE_E2E_NODE to pin the sandbox to this node")
    if not args.master_url:
        parser.error("set --master-url or CUBE_MASTER_URL")
    if args.expected_memory_mib <= 0:
        parser.error("--expected-memory-mib must be positive")
    return args


def build_template_payload(
    image: str,
    alias: str,
    annotation: list[str],
    memory_mib: int,
    node: str,
    request_id: str,
) -> dict:
    """Builds the ``POST /cube/template/from-image`` body.

    Kept pure so the hermetic tests can pin the wire shape without a cluster.
    """
    return {
        "requestID": request_id,
        "source_image_ref": image,
        "alias": alias,
        "writable_layer_size": "1G",
        "exposed_ports": [49999, 49983],
        "instance_type": "cubebox",
        "distribution_scope": [node],
        "container_overrides": {
            "annotations": {CMDLINE_ANNOTATION: json.dumps(annotation)},
            # Guest RAM lives on the container override: CreateTemplateFromImageReq
            # has no top-level Resources field, and jsoniter silently drops unknown
            # keys, so a top-level "resources" would fall back to the 2000 MiB
            # default and break the monitor-region assertion.
            "resources": {"cpu": "500m", "mem": f"{memory_mib}Mi"},
        },
    }


def create_template_with_annotation(master_url: str, payload: dict) -> tuple[str, str]:
    resp = requests.post(
        f"{master_url.rstrip('/')}/cube/template/from-image",
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    body = resp.json()
    ret = body.get("ret") or {}
    if ret.get("ret_code") != 200:
        raise RuntimeError(f"template create failed: {body}")
    job = body.get("job") or {}
    template_id = job.get("template_id")
    job_id = job.get("job_id")
    if not template_id or not job_id:
        raise RuntimeError(f"template create response missing ids: {body}")
    return str(template_id), str(job_id)


def wait_for_template_ready(master_url: str, job_id: str, timeout: float) -> None:
    terminal_failure = {"ERROR", "FAILED", "CANCELED", "CANCELLED"}

    def probe() -> bool | None:
        resp = requests.get(
            f"{master_url.rstrip('/')}/cube/template/from-image",
            params={"job_id": job_id},
            timeout=30,
        )
        resp.raise_for_status()
        body = resp.json()
        ret = body.get("ret") or {}
        if ret.get("ret_code") != 200:
            raise harness.FatalProbeError(f"template status failed: {body}")
        job = body.get("job") or {}
        status = str(job.get("status", "")).upper()
        if status in terminal_failure:
            raise harness.FatalProbeError(
                f"template build failed: status={status} phase={job.get('phase')}"
            )
        if status in {"READY", "SUCCESS", "SUCCEEDED"}:
            return True
        return None

    wait_for("temporary template build", timeout, probe, interval=3.0)


def damon_monitor_region(mem_bytes: int, machine: str | None = None) -> tuple[int, int] | None:
    """Mirrors Cubelet's ``DamonMonitorRegion`` for the worker arch."""
    gib = 1 << 30
    if mem_bytes <= 0:
        return None
    arch = (machine or platform.machine()).lower()
    if arch in {"x86_64", "amd64"}:
        end = mem_bytes if mem_bytes <= 3 * gib else mem_bytes + gib
        return 4096, end
    if arch in {"aarch64", "arm64"}:
        ram_start = 0x4000_0000
        hole_start = 0xFC00_0000
        ram64_start = 0x1_0000_0000
        ram32bit_space = hole_start - ram_start
        if mem_bytes <= ram32bit_space:
            end = ram_start + mem_bytes
        else:
            end = ram64_start + (mem_bytes - ram32bit_space)
        return ram_start, end
    return None


def assert_guest_cmdline(
    cmdline: str,
    *,
    want_start: int,
    want_end: int,
) -> None:
    required = (
        *DAMON_PARAMS,
        f"damon_reclaim.monitor_region_start={want_start}",
        f"damon_reclaim.monitor_region_end={want_end}",
    )
    tokens = set(cmdline.split())
    missing = [param for param in required if param not in tokens]
    if missing:
        raise AssertionError(f"/proc/cmdline missing {missing}: {cmdline!r}")


def read_guest_cmdline(sandbox: Sandbox, timeout: float) -> str:
    result = sandbox.commands.run("cat /proc/cmdline", timeout=timeout)
    if result.exit_code != 0:
        raise RuntimeError(
            f"reading /proc/cmdline failed with exit code {result.exit_code}: {result.stderr}"
        )
    return result.stdout


def assert_guest_kernel(sandbox: Sandbox, expect: str, timeout: float) -> None:
    """Asserts the booted guest is the expected kernel build."""
    command = KERNEL_PROBES.get(expect)
    if command is None:
        return
    result = sandbox.commands.run(command, timeout=timeout)
    if result.exit_code != 0:
        raise AssertionError(
            f"guest kernel does not match --expect-kernel {expect!r}: "
            f"`{command}` exited {result.exit_code}: {result.stderr}"
        )


def main() -> int:
    args = parse_args()
    roots = tuple(args.runtime_state_root or DEFAULT_RUNTIME_STATE_ROOTS)
    config = harness.Config()
    sandbox: Sandbox | None = None
    built_template_id: str | None = None
    result: dict[str, object] = {
        "node": args.node,
        "expected_memory_mib": args.expected_memory_mib,
        "expect_kernel": args.expect_kernel,
        "status": "FAIL",
    }
    try:
        region = damon_monitor_region(args.expected_memory_mib << 20)
        if region is None:
            raise RuntimeError(
                f"unsupported worker arch {platform.machine()!r} for monitor region"
            )
        want_start, want_end = region
        result["monitor_region_start"] = want_start
        result["monitor_region_end"] = want_end

        alias = f"cmdline-append-e2e-{int(time.time())}"
        payload = build_template_payload(
            image=args.image,
            alias=alias,
            annotation=list(DAMON_PARAMS),
            memory_mib=args.expected_memory_mib,
            node=args.node,
            request_id=f"cmdline-e2e-{int(time.time() * 1000)}",
        )
        log(f"building annotated template {alias} through CubeMaster")
        built_template_id, job_id = create_template_with_annotation(args.master_url, payload)
        result["template_id"] = built_template_id
        result["job_id"] = job_id
        wait_for_template_ready(args.master_url, job_id, args.template_build_timeout)

        log("creating a node-pinned sandbox from the annotated template")
        sandbox = Sandbox.create(
            template=built_template_id,
            timeout=900,
            distribution_scope=[args.node],
            config=config,
        )
        sandbox_id = sandbox.sandbox_id
        result["sandbox_id"] = sandbox_id
        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)

        cmdline = read_guest_cmdline(sandbox, args.poll_timeout)
        result["cmdline"] = cmdline
        assert_guest_cmdline(
            cmdline,
            want_start=want_start,
            want_end=want_end,
        )
        assert_guest_kernel(sandbox, args.expect_kernel, args.poll_timeout)
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
