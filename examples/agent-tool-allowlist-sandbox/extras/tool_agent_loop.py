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
  - /health ``sandboxes`` field is sampled as a smoke check (CubeAPI currently
    returns 0 unconditionally — this does not prove gate correctness alone)
  - A deny *after* create proves the gate still runs mid-session
  - Airgap probe: curl temporarily allowlisted only to show egress still drops
    (base image usually ships curl; skip branch is defensive)
  - Workspace artifact via allowlisted commands only (no sandbox.files.*)
  - Fail-closed assertions on counts and key observations

Trust boundary (honest): this gate wraps command strings you choose to
check. SDK surfaces such as sandbox.files.* / sandbox.run_code() are a
*separate* trust face — do not treat argv allowlisting as covering them.
"""

from __future__ import annotations

import _path  # noqa: F401  # example root on sys.path

import json
import os
import urllib.error
import urllib.request

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import (
    AllowlistDenied,
    assert_allowlisted,
    coerce_exit_code,
    exit_code_from_exc,
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
        if "sandboxes" not in data:
            raise SystemExit(f"health payload missing sandboxes: {data!r}")
        return int(data["sandboxes"])
    except (urllib.error.URLError, json.JSONDecodeError, ValueError, TypeError) as exc:
        # urlopen(timeout=…) surfaces timeouts as URLError, not TimeoutError.
        raise SystemExit(f"health check failed: {exc}") from exc


def run_gated(
    sandbox: Sandbox,
    command: str,
    *,
    extra_binaries: frozenset[str] | None = None,
    allow_unsafe_allowlist_extension: bool = False,
    allow_nonzero: bool = False,
) -> tuple[str, int]:
    assert_allowlisted(
        command,
        extra_binaries=extra_binaries,
        allow_unsafe_allowlist_extension=allow_unsafe_allowlist_extension,
    )
    try:
        result = sandbox.commands.run(command)
    except Exception as exc:
        # Only absorb SDK *command* failures when explicitly allowed.
        # Require a numeric exit_code so unrelated Exceptions still propagate.
        # (KeyboardInterrupt/SystemExit are BaseException — not caught here.)
        if not (allow_nonzero and hasattr(exc, "exit_code")):
            raise
        code = exit_code_from_exc(exc)
        stdout = (getattr(exc, "stdout", None) or "").strip()
        return stdout, code
    return result.stdout.strip(), coerce_exit_code(
        result.exit_code, what="command result"
    )



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
    # CubeAPI historically returns sandboxes=0 always; keep the equality check
    # as a forward-compatible smoke signal, not a gate proof.
    print("check: sandboxes field unchanged through early denies (smoke)")

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

    # Airgap proof when curl is present (cubesandbox-base normally ships it).
    # Skip branch remains for custom bases that omit curl.
    print("\n--- turn: airgap_probe ---")
    probe = "curl -s --max-time 3 https://example.com -o /dev/null"
    print(f"propose: {probe!r}")
    print("note: curl via allow_unsafe_allowlist_extension (not default API)")
    out, code = run_gated(
        sandbox,
        probe,
        extra_binaries=frozenset({"curl"}),
        allow_unsafe_allowlist_extension=True,
        allow_nonzero=True,
    )
    print(f"observation: stdout={out!r} exit={code}")
    if code == 127 or "not found" in out.lower():
        print("check: airgap probe skipped (curl not in image)")
        saw_airgap = True
    elif code == 0:
        raise SystemExit("airgap probe unexpectedly reached network (curl exit 0)")
    else:
        allowed += 1
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
# allow turns: hello, uname, write, read; +1 if curl airgap ran
if allowed not in (4, 5):
    raise SystemExit(f"expected allowed=4 or 5, got {allowed}")
if not (saw_hello and saw_uname and saw_artifact and saw_airgap):
    raise SystemExit("missing expected observations")

print(f"\nsummary: denied={denied} allowed={allowed}")
print("trust_boundary: gate covers checked argv strings only; "
      "sandbox.files.* / run_code are out of scope for this demo")
print("AGENT_LOOP_OK")
