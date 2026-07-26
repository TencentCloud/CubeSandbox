# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import csv
import json
import os
import re
import shutil
import sys
import tarfile
from pathlib import Path

import httpx
from dotenv import load_dotenv


EXAMPLE_DIR = Path(__file__).resolve().parent
SIDECAR_DIR = EXAMPLE_DIR.parent / "e2b-dev-sidecar"
if str(SIDECAR_DIR) not in sys.path:
    sys.path.insert(0, str(SIDECAR_DIR))

from dev_sidecar import setup_dev_sidecar


REMOTE_WORKDIR = "/tmp/cubesandbox-incident-rca"
REMOTE_ARCHIVE = "/tmp/cubesandbox-incident-rca-results.tar.gz"
LOCAL_OUTPUT_DIR = EXAMPLE_DIR / "output"
LOCAL_ARCHIVE = LOCAL_OUTPUT_DIR / "cubesandbox-incident-rca-results.tar.gz"
SNAPSHOT_NAME = "incident-rca-round1-checkpoint"
DYNAMIC_PACKAGES = ("emoji==2.14.1", "humanize==4.11.0")
SANDBOX_ID_RE = re.compile(r"^[0-9a-f]{32}$")
SANDBOX_ID_WITH_CLIENT_RE = re.compile(r"^([0-9a-f]{32})-[0-9a-f-]{36}$")


def _load_environment() -> None:
    env_path = EXAMPLE_DIR / ".env"
    load_dotenv(env_path if env_path.exists() else EXAMPLE_DIR / "env.example")


def _write_sandbox_text_file(sandbox, remote_path: str, local_path: Path) -> None:
    sandbox.files.write(remote_path, local_path.read_text(encoding="utf-8"))


def _cube_api_headers() -> dict[str, str]:
    headers = {"content-type": "application/json"}
    api_key = os.environ.get("E2B_API_KEY", "").strip()
    if api_key:
        headers["x-api-key"] = api_key
    return headers


def _cube_sandbox_id(sandbox_id: str) -> str:
    """Return the CubeAPI sandbox id from the SDK's optional client-suffixed id."""
    if SANDBOX_ID_RE.fullmatch(sandbox_id):
        return sandbox_id
    match = SANDBOX_ID_WITH_CLIENT_RE.fullmatch(sandbox_id)
    if match:
        return match.group(1)
    raise ValueError(f"Unexpected Cube sandbox id format: {sandbox_id}")


def _create_checkpoint_snapshot(api_url: str, sandbox_id: str) -> str:
    sandbox_id = _cube_sandbox_id(sandbox_id)
    response = httpx.post(
        f"{api_url.rstrip('/')}/sandboxes/{sandbox_id}/snapshots",
        headers=_cube_api_headers(),
        json={"name": SNAPSHOT_NAME},
        timeout=240,
    )
    response.raise_for_status()
    payload = response.json()
    snapshot_id = payload.get("snapshotID") or payload.get("snapshot_id")
    if not snapshot_id:
        raise RuntimeError(f"Snapshot response did not include snapshotID: {payload}")
    return snapshot_id


def _delete_checkpoint_snapshot(api_url: str, snapshot_id: str) -> None:
    response = httpx.delete(
        f"{api_url.rstrip('/')}/templates/{snapshot_id}",
        headers=_cube_api_headers(),
        timeout=240,
    )
    if response.status_code == 404:
        return
    response.raise_for_status()


def _safe_extract_tar(tar: tarfile.TarFile, target_dir: Path) -> None:
    target_root = target_dir.resolve()
    for member in tar.getmembers():
        if member.issym() or member.islnk():
            raise RuntimeError(f"Archive member uses an unsafe link: {member.name}")
        if not (member.isfile() or member.isdir()):
            raise RuntimeError(f"Archive member has an unsupported type: {member.name}")
        member_path = (target_dir / member.name).resolve()
        if target_root != member_path and target_root not in member_path.parents:
            raise RuntimeError(f"Archive member escapes output directory: {member.name}")

    if sys.version_info >= (3, 12):
        tar.extractall(target_dir, filter="data")
    else:
        tar.extractall(target_dir)


def _download_archive(sandbox) -> None:
    LOCAL_OUTPUT_DIR.mkdir(exist_ok=True)
    temp_archive = LOCAL_ARCHIVE.with_name(f"{LOCAL_ARCHIVE.name}.tmp")
    temp_archive.unlink(missing_ok=True)
    try:
        with httpx.stream("GET", sandbox.download_url(REMOTE_ARCHIVE), timeout=60) as response:
            response.raise_for_status()
            with open(temp_archive, "wb") as f:
                for chunk in response.iter_raw():
                    f.write(chunk)
    except Exception:
        temp_archive.unlink(missing_ok=True)
        raise
    temp_archive.replace(LOCAL_ARCHIVE)

    extract_dir = LOCAL_OUTPUT_DIR / "results"
    if extract_dir.exists():
        shutil.rmtree(extract_dir)
    extract_dir.mkdir()
    with tarfile.open(LOCAL_ARCHIVE, "r:gz") as tar:
        _safe_extract_tar(tar, extract_dir)

    expected = [
        "事故分析图.png",
        "incident_report.md",
        "slo_summary.csv",
        "deployment_correlation.json",
        "manifest.json",
        "state/anomaly_windows.csv",
        "state/baseline.json",
        "state/checkpoint_snapshot_id.txt",
    ]
    missing = [name for name in expected if not (extract_dir / name).exists()]
    if missing:
        raise RuntimeError(f"Downloaded archive is missing files: {missing}")

    manifest = json.loads((extract_dir / "manifest.json").read_text(encoding="utf-8"))
    deployment_correlation = json.loads(
        (extract_dir / "deployment_correlation.json").read_text(encoding="utf-8")
    )
    baseline = json.loads((extract_dir / "state/baseline.json").read_text(encoding="utf-8"))
    with open(extract_dir / "slo_summary.csv", "r", encoding="utf-8-sig", newline="") as f:
        slo_rows = list(csv.DictReader(f))
    required_slo_columns = {"metric", "baseline", "incident_peak", "threshold", "increase_ratio"}
    if not slo_rows or not required_slo_columns.issubset(slo_rows[0]):
        raise RuntimeError("Downloaded slo_summary.csv does not match the expected schema")
    if not deployment_correlation.get("likely_trigger") or not deployment_correlation.get("matched_deployments"):
        raise RuntimeError("Downloaded deployment_correlation.json is missing correlation details")
    if baseline.get("service") != "checkout-api" or not baseline.get("first_breach"):
        raise RuntimeError("Downloaded baseline.json does not match the expected incident state")
    if (
        manifest.get("scenario") != "ai_incident_rca_code_interpreter"
        or not manifest.get("likely_trigger")
        or not manifest.get("checkpoint_snapshot_id")
        or "snapshot_checkpoint_fork" not in manifest.get("stateful_rounds", [])
        or "round2_followup_rca_packaging" not in manifest.get("stateful_rounds", [])
    ):
        raise RuntimeError("Downloaded manifest does not match the expected incident RCA output")


def main() -> None:
    _load_environment()
    setup_dev_sidecar()

    from e2b import Sandbox

    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    api_url = os.environ.get("E2B_API_URL", "http://127.0.0.1:13000")

    if not template_id:
        raise RuntimeError("Please set CUBE_TEMPLATE_ID in .env or your shell environment.")

    print(f"Connecting to Cube Sandbox API at: {api_url}")
    print(f"Booting incident RCA code interpreter sandbox from template: {template_id}")

    snapshot_id = ""
    with Sandbox(template=template_id) as sandbox:
        print(f"[Success] Sandbox {sandbox.sandbox_id} is running")

        print("\n--- Step 1: Upload incident inputs and analysis programs ---")
        sandbox.commands.run(f"mkdir -p {REMOTE_WORKDIR}")
        for filename in [
            "incident_metrics.csv",
            "deployments.json",
            "alerts.csv",
            "runbook.json",
            "round1_detect.py",
            "round2_rca.py",
        ]:
            _write_sandbox_text_file(sandbox, f"{REMOTE_WORKDIR}/{filename}", EXAMPLE_DIR / filename)
        print("Uploaded metrics, deployments, alerts, runbook, and two analysis rounds")

        print("\n--- Step 2: Dynamically install follow-up analysis dependencies ---")
        packages = " ".join(DYNAMIC_PACKAGES)
        install = sandbox.commands.run(f"pip3 install --no-cache-dir {packages}")
        if install.exit_code != 0:
            print(install.stderr)
            raise RuntimeError(f"Failed to install dynamic dependencies: {packages}")
        print(f"[Success] Installed {packages} inside the running sandbox")

        print("\n--- Step 3: Round 1 anomaly detection writes sandbox state ---")
        round1 = sandbox.commands.run(f"python3 {REMOTE_WORKDIR}/round1_detect.py")
        if round1.exit_code != 0:
            print(round1.stdout)
            print(round1.stderr)
            raise RuntimeError(f"Round 1 failed with exit code {round1.exit_code}")
        print(round1.stdout)

        print("\n--- Step 4: Snapshot the round 1 workspace as a reusable checkpoint ---")
        snapshot_id = _create_checkpoint_snapshot(api_url, sandbox.sandbox_id)
        print(f"[Success] Snapshot checkpoint created: {snapshot_id}")

    try:
        print("\n--- Step 5: Fork a fresh sandbox from the checkpoint snapshot ---")
        with Sandbox(template=snapshot_id) as forked_sandbox:
            print(f"[Success] Forked sandbox {forked_sandbox.sandbox_id} from checkpoint")
            marker = forked_sandbox.commands.run(
                f"printf '%s\\n' {snapshot_id!r} > {REMOTE_WORKDIR}/state/checkpoint_snapshot_id.txt"
            )
            if marker.exit_code != 0:
                print(marker.stdout)
                print(marker.stderr)
                raise RuntimeError("Failed to write checkpoint marker in forked sandbox")

            state_check = forked_sandbox.commands.run(
                f"test -s {REMOTE_WORKDIR}/state/baseline.json "
                f"&& test -s {REMOTE_WORKDIR}/state/anomaly_windows.csv"
            )
            if state_check.exit_code != 0:
                print(state_check.stdout)
                print(state_check.stderr)
                raise RuntimeError("Forked sandbox did not inherit round 1 analysis state")
            print("[Success] Forked sandbox inherited the round 1 analysis state")

            print("\n--- Step 6: Round 2 follow-up RCA reuses forked state and packages artifacts ---")
            round2 = forked_sandbox.commands.run(f"python3 {REMOTE_WORKDIR}/round2_rca.py")
            if round2.exit_code != 0:
                print(round2.stdout)
                print(round2.stderr)
                raise RuntimeError(f"Round 2 failed with exit code {round2.exit_code}")
            print(round2.stdout)

            print("\n--- Step 7: Download and extract packaged RCA results ---")
            _download_archive(forked_sandbox)
            print(f"[Success] Archive downloaded to: {LOCAL_ARCHIVE}")
            print(f"[Success] Extracted files to: {LOCAL_OUTPUT_DIR / 'results'}")
    finally:
        if snapshot_id:
            print("\n--- Cleanup: Delete checkpoint snapshot ---")
            has_active_error = sys.exc_info()[0] is not None
            try:
                _delete_checkpoint_snapshot(api_url, snapshot_id)
                print(f"[Success] Snapshot deleted: {snapshot_id}")
            except Exception as cleanup_error:
                print(f"[WARN] Failed to delete snapshot {snapshot_id}: {cleanup_error}")
                if not has_active_error:
                    raise

    print("\n[Finished] Incident RCA code interpreter flow completed successfully")


if __name__ == "__main__":
    main()
