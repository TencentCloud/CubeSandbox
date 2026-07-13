"""Mock implementations of e2b and cubesandbox SDKs for offline verification."""

from __future__ import annotations

import re
import uuid
import time
from typing import Any


# ── Shared helpers ──────────────────────────────────────────────────────────

class MockCommandResult:
    def __init__(self, exit_code: int, stdout: str = "", stderr: str = ""):
        self.exit_code = exit_code
        self.stdout = stdout
        self.stderr = stderr


class MockSnapshotInfo:
    def __init__(self, snapshot_id: str, name: str | None = None):
        self.snapshot_id = snapshot_id
        self.name = name


# ── In-memory filesystem ────────────────────────────────────────────────────

class MockFiles:
    def __init__(self) -> None:
        self._store: dict[str, str | bytes] = {}

    def write(self, path: str, content: str | bytes) -> None:
        self._store[path] = content

    def read(self, path: str, format: str = "text") -> str | bytes:
        content = self._store.get(path, "")
        if isinstance(content, bytes):
            if format == "text":
                return content.decode("utf-8", errors="replace")
            return content
        if isinstance(content, str):
            if format == "bytes":
                return content.encode("utf-8")
            return content
        return content

    def exists(self, path: str) -> bool:
        return path in self._store

    def snapshot(self) -> dict[str, str | bytes]:
        return dict(self._store)

    def restore(self, state: dict[str, str | bytes]) -> None:
        self._store = dict(state)


# ── Mock command runner ─────────────────────────────────────────────────────

_KNOWN_CRATES = {
    "serde_json": "1.0.128",
    "chrono": "0.4.38",
}


class MockCommands:
    def __init__(self, files: MockFiles, sandbox: Any = None) -> None:
        self._files = files
        self._sandbox = sandbox
        self._log: list[str] = []

    def run(self, command: str, *, cwd: str | None = None, timeout: int = 60) -> MockCommandResult:
        self._log.append(f"$ {command}  (cwd={cwd})")

        cmd = command.strip()
        # Strip CARGO_ENV / RUSTUP_HOME prefixes used by snapshot_driven_dev
        cmd = re.sub(r'^(CARGO_HOME=\S+\s+)?(RUSTUP_HOME=\S+\s+)?(HOME=\S+\s+)?', '', cmd).strip()

        # mkdir
        if cmd.startswith("mkdir"):
            return MockCommandResult(0, "", "")

        # rustc compile
        if cmd.startswith("rustc "):
            src_name = cmd.removeprefix("rustc ").strip()
            content = self._files._store.get(f"/home/user/workspace/{src_name}", "")
            if 'fn main()' in content:
                ec, out, err = 0, "", ""
            else:
                ec, out, err = 1, "", "error[E0601]: `main` function not found"
            return MockCommandResult(ec, out, err)

        # cargo build
        if cmd.startswith("cargo build"):
            is_release = "--release" in cmd
            cargo_toml = self._files._store.get(f"{cwd}/Cargo.toml" if cwd else "", "")
            main_rs = self._files._store.get(f"{cwd}/src/main.rs" if cwd else "", "")
            if 'fn main()' not in main_rs:
                return MockCommandResult(1, "",
                    "error[E0601]: `main` function not found in crate")

            has_deps = any(dep in cargo_toml for dep in _KNOWN_CRATES)

            # Network isolation check: if offline and deps are needed, fail
            if has_deps and self._sandbox is not None and not self._sandbox.allow_internet_access:
                return MockCommandResult(101, "",
                    "error: failed to download serde_json v1.0\n"
                    "Caused by: cannot fetch crates.io — network is disabled\n"
                    "  (allow_internet_access=False)")

            if has_deps:
                out = ("    Updating crates.io index\n"
                       "  Downloaded serde_json v1.0.128\n"
                       "  Downloaded chrono v0.4.38\n"
                       f"   Compiling sandbox-demo v0.1.0\n"
                       f"    Finished `release` profile [optimized] target(s) in 15.32s\n")
            else:
                profile = "release" if is_release else "dev"
                bin_name = cwd.rstrip("/").rsplit("/", 1)[-1] if cwd else "snapshot-demo"
                out = (f"   Compiling {bin_name} v0.1.0\n"
                       f"    Finished `{profile}` profile [unoptimized + debuginfo] target(s) in 3.21s\n")

            if self._sandbox is not None:
                self._sandbox._binary_built = True
                # Write a dummy binary to _store so files.read() can find it
                if cwd:
                    binary_path = f"{cwd}/target/release/{cwd.rstrip('/').rsplit('/', 1)[-1]}"
                    self._files._store[binary_path] = "mock-binary-content"
                    debug_path = f"{cwd}/target/debug/{cwd.rstrip('/').rsplit('/', 1)[-1]}"
                    self._files._store[debug_path] = "mock-binary-content-debug"
            return MockCommandResult(0, out, "")

        # ./target/release/sandbox-demo
        if "./target/release/sandbox-demo" in cmd:
            main_rs = self._files._store.get(f"{cwd}/src/main.rs" if cwd else "", "")
            if 'serde_json' in str(self._files._store):
                return MockCommandResult(0,
                    '{"greeting":"Hello from CubeSandbox!","language":"Rust",'
                    '"timestamp":"2026-07-10T12:00:00+00:00",'
                    '"crates":["serde_json","chrono"],"answer":42}\n', "")
            return MockCommandResult(0, "release binary output\n", "")

        # /home/user/workspace/runner/multi-container-demo
        if "multi-container-demo" in cmd and not cmd.startswith(("cargo", "rustc")):
            return MockCommandResult(0,
                '{\n  "service": "builder",\n  "status": "compiled",\n'
                '  "timestamp": "2026-07-10T12:00:00+00:00",\n'
                '  "version": "0.1.0"\n}\n', "")

        # ./target/debug/snapshot-demo
        if "./target/debug/snapshot-demo" in cmd:
            if self._sandbox is not None and not self._sandbox._binary_built:
                return MockCommandResult(127, "", "bash: line 1: ./target/debug/snapshot-demo: No such file or directory")
            main_rs = self._files._store.get(
                f"{cwd}/src/main.rs" if cwd else "",
                ""
            )
            if "Clone fork" in main_rs:
                out = "Clone fork: modified version with extra features\nSum of 1..100 = 5050\n"
            elif "Checkpoint B" in main_rs:
                out = "Checkpoint B: modified version with extra features\nSum of 1..100 = 5050\n"
            elif "Checkpoint A" in main_rs:
                out = "Checkpoint A: original version\n"
            else:
                out = f"snapshot-demo binary output\n"
            return MockCommandResult(0, out, "")

        # ./hello (rustc binary)
        if cmd.startswith("./hello"):
            return MockCommandResult(0,
                "Hello from CubeSandbox Rust playground!\n"
                f"Current time: {int(time.time())}\n",
                "")

        # chmod
        if cmd.startswith("chmod"):
            return MockCommandResult(0, "", "")

        # echo $SESSION_ID
        if cmd.startswith("echo"):
            return MockCommandResult(0, "", "")

        return MockCommandResult(0, "", "")


# ── Mock Sandbox (works for both e2b and cubesandbox APIs) ──────────────────

_SANDBOX_COUNTER = 0
# Global snapshot store so list_snapshots() classmethod can find snapshots
_GLOBAL_SNAPSHOTS: dict[str, dict[str, Any]] = {}

class MockSandbox:
    def __init__(self, template: str = "", timeout: int = 120,
                 envs: dict[str, str] | None = None,
                 lifecycle: dict[str, Any] | None = None,
                 allow_internet_access: bool = True):
        global _SANDBOX_COUNTER
        _SANDBOX_COUNTER += 1
        self.sandbox_id = f"sb-{_SANDBOX_COUNTER:06d}"
        self.id = self.sandbox_id
        self.template = template
        self.timeout = timeout
        self.envs = envs or {}
        self.lifecycle = lifecycle or {}
        self.allow_internet_access = allow_internet_access
        self._files = MockFiles()
        self._snapshot_ids: list[str] = []
        self._state = "running"
        self._log: list[str] = []
        self._binary_built = False

        self.files = self._files
        self.commands = MockCommands(self._files, sandbox=self)

    class MockInfoState:
        def __init__(self, value: str):
            self.value = value

    def get_info(self) -> MockSandboxInfo:
        class MockSandboxInfo:
            sandbox_id: str
            state: "MockSandbox.MockInfoState"
            template: str
            timeout: int
        info = MockSandboxInfo()
        info.sandbox_id = self.sandbox_id
        info.state = self.MockInfoState(self._state)
        info.template = self.template
        info.timeout = self.timeout
        return info

    def create_snapshot(self, name: str | None = None) -> MockSnapshotInfo:
        snap_id = f"snap-{uuid.uuid4().hex[:12]}"
        snap_state = {
            "files": self._files.snapshot(),
            "sandbox_id": self.sandbox_id,
            "name": name,
            "binary_built": self._binary_built,
        }
        _GLOBAL_SNAPSHOTS[snap_id] = snap_state
        self._snapshot_ids.append(snap_id)
        return MockSnapshotInfo(snapshot_id=snap_id, name=name)

    def rollback(self, snapshot_id: str) -> None:
        if snapshot_id in _GLOBAL_SNAPSHOTS:
            state = _GLOBAL_SNAPSHOTS[snapshot_id]
            self._files.restore(state["files"])
            self._state = "running"

    def clone(self, n: int = 1) -> list[MockSandbox]:
        clones = []
        for _ in range(n):
            sb = MockSandbox(template=self.template, timeout=self.timeout)
            sb._files = MockFiles()
            for k, v in self._files._store.items():
                sb._files._store[k] = v
            sb._binary_built = self._binary_built
            clones.append(sb)
        return clones

    def kill(self) -> None:
        self._state = "killed"

    @classmethod
    def create(cls, template: str = "", timeout: int = 120,
               envs: dict[str, str] | None = None,
               lifecycle: dict[str, Any] | None = None,
               allow_internet_access: bool = True) -> MockSandbox:
        sb = cls(template=template, timeout=timeout, envs=envs,
                 lifecycle=lifecycle, allow_internet_access=allow_internet_access)
        # If template is a snapshot ID, restore that snapshot's files and mark binary built
        if template in _GLOBAL_SNAPSHOTS:
            state = _GLOBAL_SNAPSHOTS[template]
            sb._files.restore(state["files"])
            sb._binary_built = state.get("binary_built", False)
        return sb

    class MockSnapshotPager:
        def __init__(self, items: list[MockSnapshotInfo]):
            self._items = items
            self._index = 0

        @property
        def has_next(self) -> bool:
            return self._index < len(self._items)

        def next_items(self) -> list[MockSnapshotInfo]:
            remaining = self._items[self._index:]
            self._index = len(self._items)
            return remaining

    @classmethod
    def list_snapshots(cls, sandbox_id: str | None = None,
                       next_token: str | None = None) -> "MockSandbox.MockSnapshotPager":
        items = []
        for snap_id, state in _GLOBAL_SNAPSHOTS.items():
            if sandbox_id is None or state.get("sandbox_id") == sandbox_id:
                items.append(MockSnapshotInfo(snapshot_id=snap_id, name=state.get("name")))
        return cls.MockSnapshotPager(items)

    @classmethod
    def delete_snapshot(cls, snapshot_id: str) -> None:
        _GLOBAL_SNAPSHOTS.pop(snapshot_id, None)

    # ── Context manager support ──────────────────────────────────────────
    def __enter__(self) -> MockSandbox:
        return self

    def __exit__(self, *exc: Any) -> None:
        self.kill()


# ── Patch sys.modules so that "from e2b import Sandbox" gets our mock ───────

def install():
    import sys
    from types import ModuleType

    mod = ModuleType("e2b")
    mod.Sandbox = MockSandbox
    sys.modules["e2b"] = mod

    # Sub-module for CommandExitException used by network_isolation
    exc_mod = ModuleType("e2b.sandbox")
    exc_mod.commands = None
    sys.modules["e2b.sandbox"] = exc_mod

    cmd_mod = ModuleType("e2b.sandbox.commands")
    cmd_mod.command_handle = None
    sys.modules["e2b.sandbox.commands"] = cmd_mod

    handle_mod = ModuleType("e2b.sandbox.commands.command_handle")
    handle_mod.CommandExitException = type("CommandExitException", (Exception,), {})
    sys.modules["e2b.sandbox.commands.command_handle"] = handle_mod
    sys.modules["cubesandbox"] = type(sys)("cubesandbox")
    sys.modules["cubesandbox"].Sandbox = MockSandbox
