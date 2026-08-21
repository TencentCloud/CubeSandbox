#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
"""Regression verification for the CubeTemplateCenter fault list.

Each fault in docs/dev/templatecenter-faults.md maps to one or more checks here.
Three layers, increasing in cost and environment requirement:

  static  grep the source for the invariants each fix relies on (a guard clause
          exists, a recover() exists, an atomic rename exists, a removed config
          key is really gone). No build, no runtime -- run anywhere.
  unit    invoke the Go tests that cover each fault (needs Linux: the image
          package uses unix.F_SETPIPE_SZ / CAP_SYS_ADMIN).
  e2e     drive the real two-way channel via scripts/e2e_templatecenter.py
          (needs a running CubeMaster, and TC for the tc scenario).

Only the standard library is used.

Usage:
  python3 scripts/verify_template_faults.py --layer static
  python3 scripts/verify_template_faults.py --layer unit
  python3 scripts/verify_template_faults.py --layer e2e \
      --master http://127.0.0.1:8089 --tc http://127.0.0.1:8090
  python3 scripts/verify_template_faults.py --layer all [--master ... --tc ...]

Exit code 0 = all selected checks passed, 1 = some failed, 2 = setup error.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys

# Repo root = parent of this script's directory.
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

CM = "CubeMaster"
TC = "CubeTemplateCenter"
DB = "CubeDB"


# ---------------------------------------------------------------------------
# Result tracking.
# ---------------------------------------------------------------------------

class Results:
    def __init__(self) -> None:
        self.rows: list[tuple[bool, str, str]] = []

    def ok(self, name: str, detail: str = "") -> None:
        self.rows.append((True, name, detail))
        print(f"  [PASS] {name}" + (f"  -- {detail}" if detail else ""))

    def fail(self, name: str, detail: str = "") -> None:
        self.rows.append((False, name, detail))
        print(f"  [FAIL] {name}" + (f"  -- {detail}" if detail else ""))

    def check(self, cond: bool, name: str, detail: str = "") -> bool:
        (self.ok if cond else self.fail)(name, detail)
        return cond

    @property
    def failed(self) -> int:
        return sum(1 for r in self.rows if not r[0])

    @property
    def passed(self) -> int:
        return sum(1 for r in self.rows if r[0])


# ---------------------------------------------------------------------------
# Static-layer primitives.
# ---------------------------------------------------------------------------

def read(rel: str) -> str:
    path = os.path.join(REPO, rel)
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except OSError:
        return ""


def exists(rel: str) -> bool:
    return os.path.exists(os.path.join(REPO, rel))


def grep_count(rel: str, pattern: str) -> int:
    return len(re.findall(pattern, read(rel)))


def contains(rel: str, needle: str) -> bool:
    return needle in read(rel)


def line_index(text: str, needle: str) -> int:
    """1-based line number of first occurrence, or -1."""
    for i, line in enumerate(text.splitlines(), 1):
        if needle in line:
            return i
    return -1


# ---------------------------------------------------------------------------
# Static checks, one per fault.
# ---------------------------------------------------------------------------

def static_checks(res: Results) -> None:
    print("\n== static layer: source invariants ==")

    # F1: BuildExt4 builds into a temp file and atomically renames into place.
    ext4 = f"{CM}/pkg/templatecenter/image/ext4.go"
    res.check(contains(ext4, ".tmp.") and grep_count(ext4, r"os\.Rename") >= 1,
              "F1 ext4 built into temp file + atomic rename",
              ext4)

    # F2: cross-process build marker guards cleanup.
    res.check(exists(f"{CM}/pkg/templatecenter/image/build_marker.go"),
              "F2 cross-process build marker file exists")
    res.check(contains(f"{CM}/pkg/templatecenter/image/build_marker.go", "build-in-progress") or
              grep_count(f"{CM}/pkg/templatecenter/image/build_marker.go", r"in.?progress") >= 1,
              "F2 build marker guards cleanup")

    # F3: three-value presence probe consulted on reuse/distribution/download.
    ap = f"{CM}/pkg/templatecenter/artifact_presence.go"
    res.check(grep_count(ap, r"artifactPresenceMissing") >= 1 and
              grep_count(ap, r"artifactPresenceUnknown") >= 1,
              "F3 three-value artifact presence probe", ap)
    res.check(contains(f"{CM}/pkg/templatecenter/artifact_build.go", "rootfsArtifactReuseVerdict") or
              contains(f"{CM}/pkg/templatecenter/artifact_build.go", "readyArtifactUsableForReuse"),
              "F3 reuse path consults presence probe")
    res.check(contains(f"{CM}/pkg/templatecenter/distribution.go", "resolveMissingArtifact"),
              "F3 distribution readiness consults presence probe")
    res.check(contains(f"{CM}/pkg/templatecenter/template_image.go", "resolveMissingArtifact"),
              "F3 download endpoint demotes on missing artifact")

    # F4: foreign artifact is surfaced, never silently rebuilt.
    res.check(contains(ap, "ErrRootfsArtifactForeign"),
              "F4 foreign-artifact sentinel error defined", ap)
    res.check(contains(ap, "artifactAuthoritativeHere"),
              "F4 ownership check decides demote vs foreign", ap)

    # F5: redo non-rebuild resume path loads the artifact (no nil deref).
    redo = read(f"{CM}/pkg/templatecenter/redo.go")
    res.check("getRootfsArtifactByID" in redo and "ImageConfigJSON" in redo,
              "F5 redo resume loads artifact before ImageConfigJSON use",
              f"{CM}/pkg/templatecenter/redo.go")

    # F6: size-match check + Unknown not reused.
    res.check(contains(ap, "rootfsArtifactSizeMatches"),
              "F6 reuse verdict checks on-disk size vs row", ap)

    # F7: distribution failure gate is ready==0 (not expected>0 && ready==0).
    res.check(contains(f"{CM}/pkg/templatecenter/image_job_runner.go", "func distributionFailure"),
              "F7 distributionFailure distinguishes untouched vs all-failed",
              f"{CM}/pkg/templatecenter/image_job_runner.go")

    # F8: remote resume goroutine recovers from panic.
    res.check(contains(f"{CM}/pkg/service/httpservice/cube/template_job_callback.go", "recover()"),
              "F8 remote resume goroutine has recover()",
              f"{CM}/pkg/service/httpservice/cube/template_job_callback.go")

    # F9: TC's BUILT report is folded into remote_build_report, not overwritten.
    res.check(contains(f"{CM}/pkg/templatecenter/remote_build_resume.go", "mergeRemoteBuildReport") or
              contains(f"{CM}/pkg/templatecenter/remote_build_resume.go", "remote_build_report"),
              "F9 remote build report preserved under remote_build_report key")

    # F11: source_image_ref guard runs BEFORE the destructive redo cleanup.
    # Anchor on the call sites, not the function definitions: the guard is the
    # fail message, the destructive step is the prepareRootfsArtifactForRedoBuild
    # CALL that wipes the prior artifact.
    redo_text = read(f"{CM}/pkg/templatecenter/redo.go")
    guard_line = line_index(redo_text, "no source_image_ref")
    cleanup_line = line_index(redo_text, "prepareRootfsArtifactForRedoBuild(ctx,")
    res.check(0 < guard_line < cleanup_line,
              "F11 source_image_ref guard precedes destructive redo cleanup",
              f"guard@{guard_line} cleanup@{cleanup_line}")

    # F12: force delete keeps the in-use check before failing in-flight work.
    # Anchor on the call sites in deleteTemplateWithTargets (runInUseCheck(ctx...)
    # must come before runFailActiveWork(ctx...)), not the var declarations.
    delete_text = read(f"{CM}/pkg/templatecenter/delete.go")
    inuse_line = line_index(delete_text, "runInUseCheck(ctx,")
    failwork_line = line_index(delete_text, "runFailActiveWork(ctx,")
    res.check(0 < inuse_line < failwork_line,
              "F12 force delete checks in-use before failing in-flight work",
              f"in-use@{inuse_line} fail-work@{failwork_line}")
    res.check(contains(f"{CM}/pkg/templatecenter/delete.go", "DeleteTemplateOptions"),
              "F12 DeleteTemplateOptions{Force} exists")

    # F13: absent migration versions warn, content drift stays fatal.
    fp = f"{DB}/migrate/fingerprint.go"
    res.check(contains(fp, "logAbsentVersions") and contains(fp, "ErrFingerprintMismatch"),
              "F13 fingerprint preflight splits absent (warn) vs drift (fatal)", fp)

    # F14: single-node TC conf is a real template (no {{ }}), uses __PLACEHOLDER__.
    tcconf = "configs/single-node/templatecenter.yaml"
    res.check(exists(tcconf) and "{{" not in read(tcconf) and "__CUBE_SANDBOX_" in read(tcconf),
              "F14 single-node TC conf has placeholders, not Helm template markers", tcconf)

    # F15: chart wires the switch and both address envs.
    res.check(contains("deploy/kubernetes/chart/files/cube-master/conf.yaml", "templatecenter_enabled"),
              "F15 chart renders templatecenter_enabled")
    res.check(contains("deploy/kubernetes/chart/templates/master.yaml", "CUBE_TEMPLATE_CENTER_ADDR"),
              "F15 chart injects CUBE_TEMPLATE_CENTER_ADDR into cubemaster")
    res.check(contains("deploy/kubernetes/chart/templates/templatecenter.yaml", "CUBE_MASTER_ADDR"),
              "F15 chart injects CUBE_MASTER_ADDR into TC")

    # F16: removed config keys are really gone from Go source.
    gone = grep_count(f"{CM}/pkg/base/config/config.go", r"template_build_mode|template_route_mode")
    res.check(gone == 0,
              "F16 template_build_mode / template_route_mode removed from config.go",
              f"{gone} residual refs")

    # F17: TC image has an ENTRYPOINT.
    res.check(contains(f"{TC}/docker/Dockerfile", "ENTRYPOINT"),
              "F17 TC Dockerfile defines ENTRYPOINT")

    # F18: no Terraform TC module remains.
    res.check(not exists("deploy/one-click/terraform/tencentcloud/templatecenter.tf"),
              "F18 terraform templatecenter.tf removed")


# ---------------------------------------------------------------------------
# Unit layer: run the Go tests that cover each fault.
# ---------------------------------------------------------------------------

UNIT_TESTS = [
    # (name, module_dir, package, -run pattern)
    ("F1/F2 image build+marker", CM, "./pkg/templatecenter/image/", "BuildMarker|Build"),
    ("F3/F4/F6 artifact presence", CM, "./pkg/templatecenter/", "ArtifactPresence|ReadyArtifactUsable|RootfsArtifact|Foreign|ServedLocally"),
    ("F5 redo resume", CM, "./pkg/templatecenter/", "TestRunRedoTemplateImageJob"),
    ("F7 distribution outcome", CM, "./pkg/templatecenter/", "DistributionOutcome"),
    ("F9 remote build report", CM, "./pkg/templatecenter/", "RemoteBuildReport"),
    ("F12 force delete", CM, "./pkg/templatecenter/", "DeleteTemplateWithTargets"),
    ("F13 migration fingerprint", DB, "./migrate/", "PreflightFingerprints"),
    ("TC executor (D)", TC, "./pkg/build/", "."),
    ("config templatecenter switch", CM, "./pkg/base/config/", "TemplateCenter|TemplateBuildRemote"),
    ("markFailed fresh ctx", CM, "./pkg/service/httpservice/cube/", "ForwardBuildJobFailed"),
    ("TC two-way channel (Go)", TC, "./e2e/", "."),
]


def go_test_available() -> bool:
    try:
        subprocess.run(["go", "version"], capture_output=True, check=True)
        return True
    except (OSError, subprocess.CalledProcessError):
        return False


def unit_checks(res: Results, verbose: bool) -> None:
    print("\n== unit layer: go tests per fault ==")
    if not go_test_available():
        res.fail("go toolchain available", "`go` not found in PATH")
        return
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    for name, module, pkg, pattern in UNIT_TESTS:
        cmd = ["go", "test", pkg, "-count=1", "-run", pattern]
        if verbose:
            cmd.append("-v")
        proc = subprocess.run(cmd, cwd=os.path.join(REPO, module),
                              capture_output=True, text=True, env=env)
        tail = (proc.stdout + proc.stderr).strip().splitlines()
        tail = tail[-3:] if tail else ["(no output)"]
        res.check(proc.returncode == 0, f"unit: {name}", " | ".join(tail))


# ---------------------------------------------------------------------------
# E2E layer: drive the real channel through e2e_templatecenter.py.
# ---------------------------------------------------------------------------

def e2e_checks(res: Results, master: str | None, tc: str | None,
               image: str, timeout: int) -> None:
    print("\n== e2e layer: real two-way channel ==")
    if not master:
        res.fail("e2e: --master provided", "e2e layer needs a running CubeMaster")
        return
    script = os.path.join(REPO, "scripts", "e2e_templatecenter.py")
    if not exists("scripts/e2e_templatecenter.py"):
        res.fail("e2e: e2e_templatecenter.py present", script)
        return

    scenarios = [("local", master, None)]
    if tc:
        scenarios.append(("tc", master, tc))
    else:
        res.ok("e2e: tc scenario skipped", "no --tc given")

    for scenario, m, t in scenarios:
        cmd = [sys.executable, script, "--master", m, "--scenario", scenario,
               "--image", image, "--timeout", str(timeout)]
        if t:
            cmd += ["--tc", t]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        tail = (proc.stdout + proc.stderr).strip().splitlines()
        tail = tail[-4:] if tail else ["(no output)"]
        res.check(proc.returncode == 0, f"e2e: {scenario} scenario", " | ".join(tail))


# ---------------------------------------------------------------------------
# Entry point.
# ---------------------------------------------------------------------------

def main() -> int:
    ap = argparse.ArgumentParser(
        description="Regression verification for the CubeTemplateCenter fault list.")
    ap.add_argument("--layer", choices=["static", "unit", "e2e", "all"],
                    default="static")
    ap.add_argument("--master", default=None, help="CubeMaster base URL (e2e)")
    ap.add_argument("--tc", default=None, help="CubeTemplateCenter base URL (e2e tc scenario)")
    ap.add_argument("--image", default="docker.io/library/busybox:latest")
    ap.add_argument("--timeout", type=int, default=300)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    res = Results()
    layers = ["static", "unit", "e2e"] if args.layer == "all" else [args.layer]

    if "static" in layers:
        static_checks(res)
    if "unit" in layers:
        unit_checks(res, args.verbose)
    if "e2e" in layers:
        e2e_checks(res, args.master, args.tc, args.image, args.timeout)

    print("\n== summary ==")
    print(f"passed: {res.passed}  failed: {res.failed}")
    return 0 if res.failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
