# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_agent_loop.py — Tiny agent-style tool dispatch with a host allowlist.

Reference loop only (not an agent framework). Hardcoded proposals stand in
for an LLM. Every turn still goes through assert_allowlisted.

Flow:
  propose → host gate → (on first allow) Sandbox.create
         → commands.run → observation

Hardening vs a naive demo:
  - Denied turns never call Sandbox.create / commands.run
  - /health sandboxes count must stay flat until the first allow
  - A deny *after* create proves the gate still runs mid-session
  - Airgap probe: curl temporarily allowlisted only to show egress still drops
  - Workspace artifact via allowlisted commands only (no sandbox.files.*)
  - Fail-closed assertions on counts and key observations

Trust boundary (honest): this gate wraps command strings you choose to
check. SDK surfaces such as sandbox.files.* / sandbox.run_code() are a
*separate* trust face — do not treat argv allowlisting as covering them.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import (
    AllowlistDenied,
    DEFAULT_ALLOWED_BINARIES,
    assert_allowlisted,
)

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]
api_url = os.environ["E2B_API_URL"].rstrip("/")


def health_sandbox_count() -> int:
    """CubeAPI /health → sandboxes field (fail closed if unreachable)."""
    req = urllib.request.Request(f"{api_url}/health", method="GET")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode())
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError, ValueError) as exc:
        raise SystemExit(f"health check failed: {exc}") from exc
    if "sandboxes" not in data:
        raise SystemExit(f"health payload missing sandboxes: {data!r}")
    return int(data["sandboxes"])


def run_gated(
    sandbox: Sandbox,
    command: str,
    *,
    allowed_binaries: frozenset[str] | None = None,
    allow_nonzero: bool = False,
) -> tuple[str, int]:
    assert_allowlisted(command, allowed_binaries=allowed_binaries)
    try:
        result = sandbox.commands.run(command)
        return result.stdout.strip(), int(result.exit_code)
    except Exception as exc:
        # e2b may raise CommandExitException from a distinct import path;
        # match by name + exit_code attribute so airgap probes can accept
        # non-zero curl timeouts without depending on class identity.
        if (
            not allow_nonzero
            or type(exc).__name__ != "CommandExitException"
            or not hasattr(exc, "exit_code")
        ):
            raise
        stdout = (getattr(exc, "stdout", None) or "").strip()
        return stdout, int(exc.exit_code)


# Pretend LLM proposals. Order is the story.
EARLY_DENY: list[tuple[str, str]] = [
    ("probe_shell", "bash -c 'id'"),
    ("exfil_curl", "curl -s http://example.com"),
]
EARLY_ALLOW: list[tuple[str, str]] = [
    ("say_hello", "echo agent-loop-ok"),
    ("guest_uname", "uname -s"),
]
MID_DENY: list[tuple[str, str]] = [
    ("mid_session_shell", "bash -c 'id'"),
]

sandbox: Sandbox | None = None
denied = 0
allowed = 0
saw_hello = False
saw_uname = False
saw_artifact = False
saw_airgap = False

print(f"api: {api_url}")
baseline = health_sandbox_count()
print(f"health.sandboxes (baseline): {baseline}")

try:
    for name, command in EARLY_DENY:
        print(f"\n--- turn: {name} ---")
        print(f"propose: {command!r}")
        try:
            assert_allowlisted(command)
        except AllowlistDenied as exc:
            denied += 1
            print(f"host_deny: {exc}")
            print("action: skip (no Sandbox.create / commands.run)")
        else:
            raise SystemExit(f"expected deny for {name}, but gate accepted")

    after_deny = health_sandbox_count()
    print(f"\nhealth.sandboxes (after early denies): {after_deny}")
    if after_deny != baseline:
        raise SystemExit(
            f"sandbox count changed during denies: {baseline} → {after_deny}"
        )
    print("check: sandboxes unchanged through early denies")

    for name, command in EARLY_ALLOW:
        print(f"\n--- turn: {name} ---")
        print(f"propose: {command!r}")
        assert_allowlisted(command)

        if sandbox is None:
            print("action: Sandbox.create (airgap, timeout=60)")
            sandbox = Sandbox.create(
                template=template_id,
                allow_internet_access=False,
                timeout=60,
            )
            sid = getattr(sandbox, "sandbox_id", None) or getattr(
                sandbox, "id", "?"
            )
            print(f"sandbox_id: {sid}")

        out, code = run_gated(sandbox, command)
        allowed += 1
        print(f"observation: {out!r} (exit={code})")
        if name == "say_hello":
            if out != "agent-loop-ok" or code != 0:
                raise SystemExit(f"unexpected hello: out={out!r} exit={code}")
            saw_hello = True
        if name == "guest_uname":
            if not out or code != 0:
                raise SystemExit(f"unexpected uname: out={out!r} exit={code}")
            saw_uname = True

    assert sandbox is not None

    for name, command in MID_DENY:
        print(f"\n--- turn: {name} ---")
        print(f"propose: {command!r}")
        try:
            assert_allowlisted(command)
        except AllowlistDenied as exc:
            denied += 1
            print(f"host_deny: {exc}")
            print("action: skip commands.run (sandbox already live)")
        else:
            raise SystemExit(f"expected mid-session deny for {name}")

    # Airgap proof: widen argv allowlist for curl only — egress must still drop.
    # No shell metacharacters (`;|&`$`) so the host gate can accept this probe.
    print("\n--- turn: airgap_probe ---")
    probe = "curl -s --max-time 3 https://example.com -o /dev/null"
    print(f"propose: {probe!r}")
    print("note: curl temporarily on allowlist to test egress, not argv deny")
    curl_allow = DEFAULT_ALLOWED_BINARIES | frozenset({"curl"})
    out, code = run_gated(
        sandbox, probe, allowed_binaries=curl_allow, allow_nonzero=True
    )
    allowed += 1
    print(f"observation: stdout={out!r} exit={code}")
    if code == 0:
        raise SystemExit("airgap probe unexpectedly reached network (curl exit 0)")
    saw_airgap = True
    print("check: airgap held (curl non-zero exit)")

    # Artifact via commands only (not sandbox.files.*).
    print("\n--- turn: write_artifact ---")
    write_cmd = "echo artifact-ok > /tmp/agent_loop.txt"
    print(f"propose: {write_cmd!r}")
    _, wcode = run_gated(sandbox, write_cmd)
    if wcode != 0:
        raise SystemExit(f"write_artifact exit={wcode}")
    allowed += 1

    print("\n--- turn: read_artifact ---")
    read_cmd = "cat /tmp/agent_loop.txt"
    print(f"propose: {read_cmd!r}")
    content, rcode = run_gated(sandbox, read_cmd)
    allowed += 1
    print(f"observation: {content!r} (exit={rcode})")
    if content != "artifact-ok" or rcode != 0:
        raise SystemExit(f"artifact mismatch: {content!r} exit={rcode}")
    saw_artifact = True

finally:
    if sandbox is not None:
        sandbox.kill()
        print("\nlifecycle: sandbox.kill()")

# Fail closed.
if denied != 3:
    raise SystemExit(f"expected denied=3, got {denied}")
if allowed != 5:
    raise SystemExit(f"expected allowed=5, got {allowed}")
if not (saw_hello and saw_uname and saw_artifact and saw_airgap):
    raise SystemExit("missing expected observations")

print(f"\nsummary: denied={denied} allowed={allowed}")
print("trust_boundary: gate covers checked argv strings only; "
      "sandbox.files.* / run_code are out of scope for this demo")
print("AGENT_LOOP_OK")
