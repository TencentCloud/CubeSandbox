# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import os
import re
import shlex
import sys
import time
from pathlib import Path

import httpx
from dotenv import load_dotenv


EXAMPLE_DIR = Path(__file__).resolve().parents[1]
SIDECAR_DIR = EXAMPLE_DIR.parent / "e2b-dev-sidecar"
if str(SIDECAR_DIR) not in sys.path:
    sys.path.insert(0, str(SIDECAR_DIR))

from dev_sidecar import setup_dev_sidecar


REMOTE_WORKDIR = "/workspace/java-springboot-web"
REMOTE_MAVEN_REPO = "/workspace/.m2/repository"
REMOTE_STATE_FILE = "/tmp/cubesandbox-spring/state/tasks.json"
REMOTE_MANIFEST = "/tmp/cubesandbox-spring/result/manifest.json"
SERVICE_PORT = 8080
SNAPSHOT_NAME = "java-springboot-maven-cache-checkpoint"
LOCAL_OUTPUT_DIR = EXAMPLE_DIR / "output"
LOCAL_MANIFEST = LOCAL_OUTPUT_DIR / "manifest.json"
# CubeProxy currently decompresses routed responses, so request identity encoding
# to keep httpx from attempting a second decompression pass.
ROUTED_HTTP_HEADERS = {"accept-encoding": "identity"}
SANDBOX_ID_RE = re.compile(r"^[0-9a-f]{32}$")
SANDBOX_ID_WITH_CLIENT_RE = re.compile(r"^([0-9a-f]{32})-[0-9a-f-]{36}$")


def _load_environment() -> None:
    env_path = EXAMPLE_DIR / ".env"
    load_dotenv(env_path if env_path.exists() else EXAMPLE_DIR / "env.example")


def _cube_api_headers() -> dict[str, str]:
    headers = {"content-type": "application/json"}
    api_key = os.environ.get("E2B_API_KEY", "").strip()
    if api_key:
        headers["x-api-key"] = api_key
    return headers


def _cube_sandbox_id(sandbox_id: str) -> str:
    if SANDBOX_ID_RE.fullmatch(sandbox_id):
        return sandbox_id
    match = SANDBOX_ID_WITH_CLIENT_RE.fullmatch(sandbox_id)
    if match:
        return match.group(1)
    raise ValueError(f"Unexpected Cube sandbox id format: {sandbox_id}")


def _create_checkpoint_snapshot(client: httpx.Client, api_url: str, sandbox_id: str) -> str:
    cube_id = _cube_sandbox_id(sandbox_id)
    response = client.post(
        f"{api_url.rstrip('/')}/sandboxes/{cube_id}/snapshots",
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


def _delete_checkpoint_snapshot(client: httpx.Client, api_url: str, snapshot_id: str) -> None:
    response = client.delete(
        f"{api_url.rstrip('/')}/templates/{snapshot_id}",
        headers=_cube_api_headers(),
        timeout=240,
    )
    if response.status_code == 404:
        return
    response.raise_for_status()


def _run_checked(sandbox, command: str, description: str) -> str:
    result = sandbox.commands.run(command)
    if result.exit_code != 0:
        print(result.stdout)
        print(result.stderr)
        raise RuntimeError(f"{description} failed with exit code {result.exit_code}")
    return result.stdout


def _maven_cache_check_command() -> str:
    return (
        f"test -d {REMOTE_MAVEN_REPO} && "
        f"find -L {REMOTE_MAVEN_REPO} -type f -print -quit | grep ."
    )


def _write_sandbox_text_file(sandbox, remote_path: str, local_path: Path) -> None:
    sandbox.files.write(remote_path, local_path.read_text(encoding="utf-8"))


def _upload_project(sandbox) -> None:
    files = [
        "pom.xml",
        "src/main/resources/application.properties",
        "src/main/java/com/example/cubesandbox/DemoApplication.java",
        "src/main/java/com/example/cubesandbox/HealthController.java",
        "src/main/java/com/example/cubesandbox/InfoController.java",
        "src/main/java/com/example/cubesandbox/CreateTaskRequest.java",
        "src/main/java/com/example/cubesandbox/TaskController.java",
        "src/main/java/com/example/cubesandbox/TaskState.java",
    ]
    directories = sorted({str(Path(REMOTE_WORKDIR) / Path(name).parent) for name in files})
    _run_checked(sandbox, "mkdir -p " + " ".join(shlex.quote(d) for d in directories), "create project directories")
    for name in files:
        _write_sandbox_text_file(sandbox, f"{REMOTE_WORKDIR}/{name}", EXAMPLE_DIR / name)


def _service_base_url(sandbox) -> str:
    return f"http://{sandbox.get_host(SERVICE_PORT)}"


def _wait_for_health(client: httpx.Client, sandbox, label: str) -> dict[str, object]:
    url = f"{_service_base_url(sandbox)}/health"
    last_error: Exception | None = None
    last_detail = "no response"
    for attempt in range(1, 61):
        try:
            response = client.get(url, timeout=2)
            if response.status_code == 200:
                payload = response.json()
                if payload.get("status") == "ok":
                    return payload
            last_detail = f"HTTP {response.status_code}: {response.text[:160]}"
        except Exception as exc:
            last_error = exc
            last_detail = f"{type(exc).__name__}: {exc}"
        if attempt == 1 or attempt % 10 == 0:
            print(f"[Wait] {label} health check {attempt}/60: {last_detail}")
        time.sleep(1)
    raise RuntimeError(f"{label} did not become healthy at {url}; last result: {last_detail}") from last_error


def _start_service(client: httpx.Client, sandbox, label: str) -> None:
    command = f"""
        cd {shlex.quote(REMOTE_WORKDIR)}
        JAR="$(ls target/*.jar | grep -v '\\.original$' | head -n1)"
        test -n "$JAR"
        nohup java $SPRING_BOOT_JAVA_OPTS -jar "$JAR" > app.log 2>&1 &
        echo $! > app.pid
    """
    _run_checked(sandbox, f"bash -lc {shlex.quote(command)}", f"start {label} Spring Boot service")
    try:
        _wait_for_health(client, sandbox, label)
    except RuntimeError:
        app_log = sandbox.commands.run(
            f"bash -lc {shlex.quote(f'cd {REMOTE_WORKDIR} && tail -n 80 app.log 2>/dev/null || true')}"
        )
        if app_log.stdout.strip():
            print(f"[Diagnostic] Last {label} Spring Boot log lines:\n{app_log.stdout.rstrip()}")
        raise


def _stop_service(sandbox, label: str) -> None:
    command = f"""
        cd {shlex.quote(REMOTE_WORKDIR)}
        if test -f app.pid; then
            pid="$(cat app.pid)"
            kill "$pid" 2>/dev/null || true
            attempts=0
            while kill -0 "$pid" 2>/dev/null && test "$attempts" -lt 30; do
                attempts=$((attempts + 1))
                sleep 1
            done
            if kill -0 "$pid" 2>/dev/null; then
                kill -KILL "$pid" 2>/dev/null || true
                sleep 1
            fi
            if kill -0 "$pid" 2>/dev/null; then
                echo "Spring Boot process $pid did not exit" >&2
                exit 1
            fi
            rm -f app.pid
        fi
    """
    _run_checked(sandbox, f"bash -lc {shlex.quote(command)}", f"stop {label} Spring Boot service")
    print(f"[Success] {label.capitalize()} Spring Boot process exited")


def _verify_egress_blocked(sandbox) -> None:
    from e2b import CommandExitException

    try:
        sandbox.commands.run(
            "curl --noproxy '*' --silent --show-error --connect-timeout 3 --max-time 5 https://example.com"
        )
    except CommandExitException as exc:
        if exc.exit_code not in {5, 6, 7, 28}:
            raise RuntimeError(
                f"Public egress probe failed for an unexpected reason (exit {exc.exit_code}): {exc.stderr}"
            ) from exc
        print("[Success] Public egress is blocked by allow_internet_access=False")
        return
    raise RuntimeError("Public egress probe unexpectedly succeeded in a default-deny sandbox")


def _download_manifest(sandbox) -> dict[str, object]:
    LOCAL_OUTPUT_DIR.mkdir(exist_ok=True)
    temp_manifest = LOCAL_MANIFEST.with_name(f"{LOCAL_MANIFEST.name}.tmp")
    temp_manifest.unlink(missing_ok=True)
    try:
        manifest_text = _run_checked(sandbox, f"cat {shlex.quote(REMOTE_MANIFEST)}", "read manifest")
        temp_manifest.write_text(manifest_text, encoding="utf-8")
    except Exception:
        temp_manifest.unlink(missing_ok=True)
        raise
    temp_manifest.replace(LOCAL_MANIFEST)
    return json.loads(LOCAL_MANIFEST.read_text(encoding="utf-8"))


def _run_workflow(client: httpx.Client, sandbox_class, template_id: str, api_url: str) -> None:
    print(f"Connecting to Cube Sandbox API at: {api_url}")
    print(f"Booting Java Spring Boot dev/test sandbox from template: {template_id}")

    snapshot_id = ""
    task_id = ""
    original_sandbox_id = ""
    forked_sandbox_id = ""
    build_seconds = 0.0

    with sandbox_class.create(template=template_id, allow_internet_access=False) as sandbox:
        original_sandbox_id = sandbox.sandbox_id
        print(f"[Success] Sandbox {original_sandbox_id} is running")

        print("\n--- Step 1: Prove default-deny egress and upload the project ---")
        _verify_egress_blocked(sandbox)

        _upload_project(sandbox)
        print(f"[Success] Project uploaded to {REMOTE_WORKDIR}")

        print("\n--- Step 2: Build with the template-warmed Maven cache ---")
        started = time.perf_counter()
        build = sandbox.commands.run(
            f"cd {REMOTE_WORKDIR} && mvn --offline -Dmaven.repo.local={REMOTE_MAVEN_REPO} -DskipTests package"
        )
        build_seconds = time.perf_counter() - started
        if build.exit_code != 0:
            print(build.stdout)
            print(build.stderr)
            raise RuntimeError(f"Maven build failed with exit code {build.exit_code}")
        print(f"[Success] Offline Maven package completed in {build_seconds:.1f}s")

        cache_check = _run_checked(
            sandbox,
            _maven_cache_check_command(),
            "verify Maven cache",
        ).strip()
        artifact_check = _run_checked(
            sandbox,
            f"cd {REMOTE_WORKDIR} && ls target/*.jar | grep -v '\\.original$' | head -n 1",
            "verify built jar",
        ).strip()
        print(f"[Success] Maven cache contains: {cache_check}")
        print(f"[Success] Built artifact: {artifact_check}")

        print("\n--- Step 3: Start Spring Boot and call it through CubeSandbox routing ---")
        _start_service(client, sandbox, "original")
        service_base = _service_base_url(sandbox)
        info_response = client.get(f"{service_base}/api/info")
        info_response.raise_for_status()
        info = info_response.json()
        print(f"[Success] /api/info returned Java {info.get('javaVersion')}")

        task_payload = {
            "title": "snapshot warmed Maven cache proof",
            "payload": {
                "scenario": "java_springboot_devtest_sandbox",
                "stateFile": REMOTE_STATE_FILE,
            },
        }
        create_response = client.post(f"{service_base}/api/tasks", json=task_payload)
        create_response.raise_for_status()
        task = create_response.json()
        task_id = str(task["id"])
        print(f"[Success] Created task through routed HTTP service: {task_id}")

        _run_checked(sandbox, f"test -s {REMOTE_STATE_FILE}", "verify task state file")
        print(f"[Success] Stateful workspace file exists: {REMOTE_STATE_FILE}")

        print("\n--- Step 4: Stop, flush persistent state, and create a checkpoint ---")
        _stop_service(sandbox, "original")
        _run_checked(sandbox, f"sync {shlex.quote(REMOTE_STATE_FILE)}", "flush task state")
        print(f"[Success] Persistent task state flushed before snapshot: {REMOTE_STATE_FILE}")
        snapshot_id = _create_checkpoint_snapshot(client, api_url, sandbox.sandbox_id)
        print(f"[Success] Snapshot checkpoint created: {snapshot_id}")

    try:
        print("\n--- Step 5: Fork a fresh sandbox from the checkpoint snapshot ---")
        with sandbox_class.create(template=snapshot_id, allow_internet_access=False) as forked_sandbox:
            forked_sandbox_id = forked_sandbox.sandbox_id
            print(f"[Success] Forked sandbox {forked_sandbox_id} from checkpoint")

            print("\n--- Step 6: Verify cache, jar, and state inheritance in the fork ---")
            _run_checked(
                forked_sandbox,
                _maven_cache_check_command(),
                "verify forked Maven cache",
            )
            _run_checked(
                forked_sandbox,
                f"cd {REMOTE_WORKDIR} && ls target/*.jar | grep -v '\\.original$' | head -n 1",
                "verify forked jar",
            )
            _run_checked(forked_sandbox, f"test -s {REMOTE_STATE_FILE}", "verify forked task state")
            print("[Success] Fork inherited Maven cache, built jar, and task state")

            print("\n--- Step 7: Start the fork directly from the inherited jar ---")
            _start_service(client, forked_sandbox, "forked")
            fork_base = _service_base_url(forked_sandbox)
            inherited_response = client.get(f"{fork_base}/api/tasks/{task_id}")
            inherited_response.raise_for_status()
            inherited_task = inherited_response.json()
            if inherited_task.get("id") != task_id:
                raise RuntimeError(f"Forked service did not return the expected task: {inherited_task}")
            print(f"[Success] Forked service read inherited task: {task_id}")

            manifest = {
                "scenario": "java_springboot_devtest_sandbox",
                "features": [
                    "spring_boot_web_service",
                    "snapshot_warmed_maven_cache",
                    "stateful_workspace_fork",
                    "default_deny_egress_offline_build",
                ],
                "original_sandbox_id": original_sandbox_id,
                "checkpoint_snapshot_id": snapshot_id,
                "forked_sandbox_id": forked_sandbox_id,
                "task_id": task_id,
                "maven_build_seconds": round(build_seconds, 3),
                "state_inherited": True,
                "artifact_inherited": True,
                "maven_cache_inherited": True,
                "service_called_through_cube_routing": True,
                "internet_access_denied": True,
                "state_flushed_before_snapshot": True,
            }
            _run_checked(forked_sandbox, "mkdir -p /tmp/cubesandbox-spring/result", "create result directory")
            forked_sandbox.files.write(REMOTE_MANIFEST, json.dumps(manifest, indent=2))
            downloaded = _download_manifest(forked_sandbox)
            if downloaded.get("task_id") != task_id or not downloaded.get("state_inherited"):
                raise RuntimeError(f"Downloaded manifest failed validation: {downloaded}")
            print(f"[Success] Downloaded manifest to: {LOCAL_MANIFEST}")
    finally:
        if snapshot_id:
            print("\n--- Cleanup: Delete checkpoint snapshot ---")
            has_active_error = sys.exc_info()[0] is not None
            try:
                _delete_checkpoint_snapshot(client, api_url, snapshot_id)
                print(f"[Success] Snapshot deleted: {snapshot_id}")
            except Exception as cleanup_error:
                print(f"[WARN] Failed to delete snapshot {snapshot_id}: {cleanup_error}")
                if not has_active_error:
                    raise

    print("\n[Finished] Java Spring Boot dev/test sandbox flow completed successfully")


def main() -> None:
    _load_environment()
    setup_dev_sidecar()

    from e2b import Sandbox

    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    api_url = os.environ.get("E2B_API_URL", "http://127.0.0.1:13000")
    if not template_id:
        raise RuntimeError("Please set CUBE_TEMPLATE_ID in .env or your shell environment.")

    with httpx.Client(headers=ROUTED_HTTP_HEADERS, timeout=10) as client:
        _run_workflow(client, Sandbox, template_id, api_url)


if __name__ == "__main__":
    main()
