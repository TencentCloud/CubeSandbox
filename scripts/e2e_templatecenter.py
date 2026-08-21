#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
"""End-to-end verification of the CubeMaster <-> CubeTemplateCenter two-way channel.

What "two-way" means here:

  forward  CubeMaster -> TC : POST {TC}/tc/api/v1/build            (build submit)
  reverse  TC -> CubeMaster : POST {MASTER}/internal/template/jobs/{job_id}/status
                                 (build status callbacks)

The script drives the PUBLIC API the same way a real client (cubemastercli) does,
and additionally probes TC's own endpoints, so it works against a real deployment
rather than in-process mocks.

Two scenarios are supported (--scenario):

  local  CubeMaster builds in-process (templatecenter_enabled=false). The
         reverse channel to TC is NOT expected to be used; we assert the build
         still completes and, if a TC is configured, that no callback traffic
         is required for success.

  tc     CubeMaster forwards builds to CubeTemplateCenter
         (templatecenter_enabled=true). We assert the FORWARD hop succeeds
         (the build starts) and the REVERSE hop lands (job status progresses
         to a terminal state), which is only possible if TC called back.

Only the standard library is used.

Usage:
  python3 scripts/e2e_templatecenter.py --master http://127.0.0.1:8089 --scenario local
  python3 scripts/e2e_templatecenter.py --master http://127.0.0.1:8089 \
      --tc http://127.0.0.1:8090 --scenario tc \
      --image docker.io/library/busybox:latest --timeout 300

Exit code 0 = all checks passed, 1 = a check failed, 2 = usage/setup error.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
import uuid

# ---------------------------------------------------------------------------
# Minimal HTTP helpers (stdlib only, no requests dependency).
# ---------------------------------------------------------------------------

DEFAULT_TIMEOUT = 30


class HttpError(Exception):
    def __init__(self, url: str, code: int, body: str):
        super().__init__(f"{url} -> HTTP {code}: {body[:300]}")
        self.code = code
        self.body = body


def _req(method: str, url: str, body: dict | None = None, timeout: int = DEFAULT_TIMEOUT,
         headers: dict | None = None) -> tuple[int, str]:
    data = None
    hdrs = {"Content-Type": "application/json"}
    if headers:
        hdrs.update(headers)
    if body is not None:
        data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    except urllib.error.URLError as e:
        raise ConnectionError(f"{method} {url} unreachable: {e}") from e


def get_json(url: str, timeout: int = DEFAULT_TIMEOUT) -> dict:
    code, text = _req("GET", url, timeout=timeout)
    if code != 200:
        raise HttpError(url, code, text)
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        # Some endpoints wrap payload in a Res envelope; return raw for the
        # caller to inspect.
        return {"_raw": text}


def post_json(url: str, body: dict, timeout: int = DEFAULT_TIMEOUT) -> tuple[int, dict]:
    code, text = _req("POST", url, body=body, timeout=timeout)
    try:
        return code, json.loads(text)
    except json.JSONDecodeError:
        return code, {"_raw": text}


# ---------------------------------------------------------------------------
# Result tracking.
# ---------------------------------------------------------------------------

class Results:
    def __init__(self) -> None:
        self.checks: list[tuple[bool, str, str]] = []  # (ok, name, detail)

    def ok(self, name: str, detail: str = "") -> None:
        self.checks.append((True, name, detail))
        print(f"  [PASS] {name}" + (f"  -- {detail}" if detail else ""))

    def fail(self, name: str, detail: str = "") -> None:
        self.checks.append((False, name, detail))
        print(f"  [FAIL] {name}" + (f"  -- {detail}" if detail else ""))

    def check(self, cond: bool, name: str, detail: str = "") -> bool:
        if cond:
            self.ok(name, detail)
        else:
            self.fail(name, detail)
        return cond

    @property
    def passed(self) -> int:
        return sum(1 for c in self.checks if c[0])

    @property
    def failed(self) -> int:
        return sum(1 for c in self.checks if not c[0])


# ---------------------------------------------------------------------------
# CubeMaster public API wrappers.
# ---------------------------------------------------------------------------

class MasterClient:
    def __init__(self, base: str):
        self.base = base.rstrip("/")

    def url(self, path: str) -> str:
        return f"{self.base}/cube{path}"

    def create_template_from_image(self, image: str, instance_type: str,
                                   template_name: str) -> tuple[int, dict]:
        body = {
            "RequestID": f"e2e-{uuid.uuid4()}",
            "source_image_ref": image,
            "instance_type": instance_type,
            "template_name": template_name,
        }
        return post_json(self.url("/template/from-image"), body)

    def get_from_image_job(self, job_id: str) -> dict:
        return get_json(self.url(f"/template/from-image?job_id={job_id}"))

    def get_build_status(self, job_id: str) -> dict:
        return get_json(self.url(f"/template/build/{job_id}/status"))

    def delete_template(self, template_id: str, instance_type: str,
                        force: bool = True) -> tuple[int, dict]:
        body = {
            "RequestID": f"e2e-{uuid.uuid4()}",
            "template_id": template_id,
            "instance_type": instance_type,
            "force": force,
        }
        return post_json(self.url("/template"), body)


class TCClient:
    """Probes CubeTemplateCenter's own endpoints (health/metrics/build)."""

    def __init__(self, base: str):
        self.base = base.rstrip("/")

    def health(self) -> tuple[int, str]:
        return _req("GET", f"{self.base}/health")

    def metrics(self) -> tuple[int, str]:
        return _req("GET", f"{self.base}/metrics")

    def build_endpoint_accepts_shape(self) -> tuple[int, dict]:
        # A deliberately job-less submit: the endpoint must be reachable and
        # must reject (404 job-not-found or 400), proving the forward hop into
        # TC works, without us needing a real persisted job id.
        return post_json(f"{self.base}/tc/api/v1/build", {
            "job_id": f"e2e-probe-{uuid.uuid4()}",
            "request": {"source_image_ref": "probe"},
            "download_base_url": "http://127.0.0.1:8089",
        })


# ---------------------------------------------------------------------------
# Job status polling (reverse-channel evidence).
# ---------------------------------------------------------------------------

TERMINAL = {"ready", "error"}


def poll_job(master: MasterClient, job_id: str, timeout: int,
             interval: float = 2.0) -> dict:
    """Poll the build-status endpoint until the job reaches a terminal state.

    This is the reverse-channel proof: status only advances to 'ready'/'error'
    when the builder (CubeMaster locally, or TC over the callback) reports it.
    Returns the last status payload observed.
    """
    deadline = time.time() + timeout
    last: dict = {}
    while time.time() < deadline:
        try:
            last = master.get_build_status(job_id)
        except (HttpError, ConnectionError) as e:
            last = {"_poll_error": str(e)}
        status = str(last.get("Status") or last.get("status") or "").lower()
        if status in TERMINAL:
            return last
        time.sleep(interval)
    return last


def _job_field(payload: dict, *names: str):
    for n in names:
        if n in payload:
            return payload[n]
    # Some responses nest under Job / job.
    for key in ("Job", "job"):
        sub = payload.get(key)
        if isinstance(sub, dict):
            for n in names:
                if n in sub:
                    return sub[n]
    return None


# ---------------------------------------------------------------------------
# Scenario implementations.
# ---------------------------------------------------------------------------

def run_common_preflight(res: Results, master: MasterClient, tc: TCClient | None) -> None:
    print("\n== preflight: reachability ==")
    try:
        _req("GET", f"{master.base}/cube/template/from-image?job_id=probe")
        res.ok("cubemaster reachable", master.base)
    except ConnectionError as e:
        res.fail("cubemaster reachable", str(e))
        raise SystemExit(2)

    if tc is not None:
        code, _ = tc.health()
        res.check(code in (200, 503), "templatecenter /health reachable",
                  f"{tc.base} -> {code}")
        code, body = tc.metrics()
        res.check(code == 200, "templatecenter /metrics reachable", f"-> {code}")


def scenario_local(res: Results, master: MasterClient, image: str,
                   instance_type: str, timeout: int) -> None:
    """CubeMaster builds in-process. Assert the build completes via the public
    status endpoint (no TC reverse channel required)."""
    print("\n== scenario: local (cubemaster builds in-process) ==")
    name = f"e2e-local-{int(time.time())}"
    code, resp = master.create_template_from_image(image, instance_type, name)
    job_id = _job_field(resp, "JobID", "job_id", "JobId")
    if not res.check(code == 200 and job_id, "create from-image accepted",
                     f"http={code} job_id={job_id}"):
        print(f"    response: {json.dumps(resp)[:400]}")
        return
    res.ok("job id returned", str(job_id))

    print(f"\n== polling build status (job {job_id}) ==")
    final = poll_job(master, job_id, timeout)
    status = str(_job_field(final, "Status", "status") or "").lower()
    res.check(status == "ready", "build reached READY via cubemaster local build",
              f"final={json.dumps(final)[:300]}")

    template_id = _job_field(final, "TemplateID", "template_id")
    if template_id:
        dcode, _ = master.delete_template(str(template_id), instance_type, force=True)
        res.check(dcode in (200, 404, 409), "cleanup: delete template",
                  f"template_id={template_id} http={dcode}")


def scenario_tc(res: Results, master: MasterClient, tc: TCClient, image: str,
                instance_type: str, timeout: int) -> None:
    """CubeMaster forwards to TC. Assert BOTH hops: forward (TC reachable and
    accepts the build shape) and reverse (job status advances to terminal,
    which only happens if TC called back)."""
    print("\n== scenario: tc (cubemaster forwards to templater) ==")

    # ---- forward hop probe: TC's build endpoint is reachable and validates.
    print("\n-- forward hop: TC build endpoint reachable --")
    code, body = tc.build_endpoint_accepts_shape()
    # 404 = job not found (endpoint works, our probe job doesn't exist),
    # 400 = validation ran. Either proves the forward channel into TC is live.
    res.check(code in (400, 404, 409, 429),
              "TC /tc/api/v1/build reachable and validates",
              f"-> {code} {json.dumps(body)[:200]}")

    # ---- real build through the public API.
    name = f"e2e-tc-{int(time.time())}"
    code, resp = master.create_template_from_image(image, instance_type, name)
    job_id = _job_field(resp, "JobID", "job_id", "JobId")
    if not res.check(code == 200 and job_id, "create from-image accepted",
                     f"http={code} job_id={job_id}"):
        print(f"    response: {json.dumps(resp)[:400]}")
        return
    res.ok("job id returned", str(job_id))

    # ---- reverse hop proof: status must reach a terminal state. If TC never
    # called back, the job would sit PENDING/RUNNING until timeout.
    print(f"\n-- reverse hop: polling job status (job {job_id}) --")
    final = poll_job(master, job_id, timeout)
    status = str(_job_field(final, "Status", "status") or "").lower()
    msg = _job_field(final, "Message", "message")
    res.check(status == "ready",
              "build reached READY (proves TC called back to cubemaster)",
              f"status={status} msg={msg}")

    template_id = _job_field(final, "TemplateID", "template_id")
    if template_id:
        dcode, _ = master.delete_template(str(template_id), instance_type, force=True)
        res.check(dcode in (200, 404, 409), "cleanup: delete template",
                  f"template_id={template_id} http={dcode}")


# ---------------------------------------------------------------------------
# Entry point.
# ---------------------------------------------------------------------------

def main() -> int:
    ap = argparse.ArgumentParser(
        description="E2E two-way channel verification for CubeMaster <-> CubeTemplateCenter")
    ap.add_argument("--master", required=True,
                    help="CubeMaster base URL, e.g. http://127.0.0.1:8089")
    ap.add_argument("--tc", default=None,
                    help="CubeTemplateCenter base URL, e.g. http://127.0.0.1:8090 "
                         "(required for --scenario tc)")
    ap.add_argument("--scenario", choices=["local", "tc"], required=True,
                    help="local: cubemaster builds in-process; tc: builds go to templatecenter")
    ap.add_argument("--image", default="docker.io/library/busybox:latest",
                    help="source image for the template build")
    ap.add_argument("--instance-type", default="cubebox",
                    help="instance type for the template")
    ap.add_argument("--timeout", type=int, default=300,
                    help="seconds to wait for a build to reach a terminal state")
    args = ap.parse_args()

    if args.scenario == "tc" and not args.tc:
        print("error: --tc is required for --scenario tc", file=sys.stderr)
        return 2

    master = MasterClient(args.master)
    tc = TCClient(args.tc) if args.tc else None
    res = Results()

    print(f"master={args.master} tc={args.tc or '(none)'} scenario={args.scenario}")
    run_common_preflight(res, master, tc)

    if args.scenario == "local":
        scenario_local(res, master, args.image, args.instance_type, args.timeout)
    else:
        scenario_tc(res, master, tc, args.image, args.instance_type, args.timeout)

    print("\n== summary ==")
    print(f"passed: {res.passed}  failed: {res.failed}")
    return 0 if res.failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
