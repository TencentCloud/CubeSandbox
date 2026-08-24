# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import inspect
import os
import time
from collections.abc import Callable
from dataclasses import asdict, is_dataclass
from enum import Enum
from typing import Any

from adapters.base import SandboxAdapter, cleanup_raw_sandbox
from framework.capabilities import E2B_CAPABILITIES
from framework.config import SdkE2EConfig
from framework.models import (
    CodeResult,
    CommandResult,
    SandboxInfo,
    first_present,
    state_from_raw,
)

MAX_LIST_PAGES = 100


def _import_e2b_sandbox():
    try:
        from e2b_code_interpreter import Sandbox  # type: ignore

        return Sandbox
    except ImportError:
        pass
    try:
        from e2b import Sandbox  # type: ignore

        return Sandbox
    except ImportError as exc:
        raise ImportError(
            "E2B backend requires e2b-code-interpreter or e2b. "
            "Install tests/e2e/sdk_compat/requirements.txt."
        ) from exc


def _import_e2b_command_exit_exception():
    try:
        from e2b.sandbox.commands.command_handle import CommandExitException  # type: ignore

        return CommandExitException
    except ImportError:
        return None


def _get_sandbox_id(sandbox: Any) -> str:
    for name in ("sandbox_id", "sandboxID", "id"):
        value = getattr(sandbox, name, None)
        if value:
            return str(value)
    data = getattr(sandbox, "_data", None)
    if isinstance(data, dict):
        value = first_present(data, "sandboxID", "sandbox_id", "id")
        if value is not None:
            return str(value)
    raise RuntimeError("could not determine E2B sandbox id")


def _normalize_info_value(value: Any) -> Any:
    if isinstance(value, Enum):
        return value.value
    if hasattr(value, "isoformat"):
        return value.isoformat()
    if is_dataclass(value):
        return {
            key: _normalize_info_value(item)
            for key, item in asdict(value).items()
        }
    if isinstance(value, dict):
        return {key: _normalize_info_value(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_normalize_info_value(item) for item in value]
    return value


def _sandbox_info_to_raw(info: Any) -> dict[str, Any]:
    """Normalize E2B SandboxInfo objects and older dict responses."""
    if info is None:
        return {}
    if isinstance(info, dict):
        raw = dict(info)
    elif is_dataclass(info):
        raw = asdict(info)
    else:
        raw = {}
        for key in (
            "sandbox_id",
            "sandboxID",
            "template_id",
            "templateID",
            "sandbox_domain",
            "domain",
            "metadata",
            "started_at",
            "startedAt",
            "end_at",
            "endAt",
            "state",
            "status",
            "cpu_count",
            "cpuCount",
            "memory_mb",
            "memoryMB",
            "envd_version",
            "envdVersion",
        ):
            if hasattr(info, key):
                raw[key] = getattr(info, key)

    normalized = {key: _normalize_info_value(value) for key, value in raw.items()}

    # Keep both E2B snake_case and CubeAPI camel/acronym-style aliases available.
    aliases = {
        "sandbox_id": "sandboxID",
        "template_id": "templateID",
        "sandbox_domain": "domain",
        "started_at": "startedAt",
        "end_at": "endAt",
        "cpu_count": "cpuCount",
        "memory_mb": "memoryMB",
        "envd_version": "envdVersion",
    }
    for source, alias in aliases.items():
        if source in normalized and alias not in normalized:
            normalized[alias] = normalized[source]

    return normalized


def _sandbox_entry_to_dict(entry: Any) -> dict[str, Any]:
    if isinstance(entry, dict):
        data = dict(entry)
    elif is_dataclass(entry) and not isinstance(entry, type):
        data = asdict(entry)
    else:
        data = {
            name: getattr(entry, name)
            for name in ("sandbox_id", "sandboxID", "id", "state", "metadata", "template_id")
            if hasattr(entry, name)
        }

    sandbox_id = first_present(data, "sandbox_id", "sandboxID", "id")
    if sandbox_id is not None:
        data.setdefault("sandbox_id", sandbox_id)
        data.setdefault("sandboxID", sandbox_id)
    state = data.get("state")
    if state is not None:
        data["state"] = str(getattr(state, "value", state))
    return data


def _list_sandboxes_via_cubeapi(config: SdkE2EConfig) -> list[dict[str, Any]]:
    from adapters.api_adapter import ApiClient

    api = ApiClient(config)
    try:
        entries = api.list_sandboxes()
    finally:
        api.close()
    return [_sandbox_entry_to_dict(entry) for entry in entries]


class E2BAdapter(SandboxAdapter):
    backend = "e2b"
    capabilities = E2B_CAPABILITIES

    @classmethod
    def create(
        cls,
        config: SdkE2EConfig,
        *,
        metadata: dict[str, str] | None = None,
        create_options: dict[str, Any] | None = None,
    ) -> "E2BAdapter":
        Sandbox = _import_e2b_sandbox()

        opts = dict(create_options or {})
        # The backend-neutral tests use the CubeSandbox SDK create option name,
        # while E2B's Python SDK calls the same concept `envs`.
        if "env_vars" in opts:
            opts.setdefault("envs", opts.pop("env_vars"))
        merged_metadata = dict(metadata or {})
        extra_metadata = opts.pop("metadata", None)
        if isinstance(extra_metadata, dict):
            merged_metadata.update(extra_metadata)

        kwargs: dict[str, Any] = {
            "template": config.cube_template_id,
            "timeout": config.create_timeout,
            "metadata": merged_metadata or None,
            **_e2b_api_params(config),
        }
        kwargs.update(opts)
        kwargs = {key: value for key, value in kwargs.items() if value is not None}
        sandbox = None
        try:
            create_method = getattr(Sandbox, "create", None)
            if callable(create_method):
                sandbox = create_method(**kwargs)
            else:
                sandbox = Sandbox(**kwargs)
            return cls(sandbox, e2e_config=config)
        except Exception:
            if sandbox is not None:
                cleanup_raw_sandbox(sandbox)
            raise

    @classmethod
    def connect(cls, sandbox_id: str, config: SdkE2EConfig, *, timeout: int | None = None) -> "E2BAdapter":
        Sandbox = _import_e2b_sandbox()
        connect_method = getattr(Sandbox, "connect", None)
        if callable(connect_method):
            kwargs = dict(_e2b_api_params(config))
            if _accepts_keyword(connect_method, "timeout"):
                kwargs["timeout"] = timeout
            sandbox = connect_method(sandbox_id, **kwargs)
        else:
            sandbox = Sandbox(sandbox_id=sandbox_id, **_e2b_api_params(config))
        return cls(sandbox, e2e_config=config)

    def __init__(self, sandbox: Any, *, e2e_config: SdkE2EConfig | None = None) -> None:
        super().__init__(sandbox)
        self._e2e_config = e2e_config

    @property
    def sandbox_id(self) -> str:
        return _get_sandbox_id(self._sandbox)

    def info(self) -> SandboxInfo:
        raw: dict[str, Any] = {}
        for name in ("get_info", "get_info_sync"):
            method = getattr(self._sandbox, name, None)
            if callable(method):
                raw = _sandbox_info_to_raw(method())
                break
        raw_sandbox_id = first_present(raw, "sandboxID", "sandbox_id")
        return SandboxInfo(
            sandbox_id=str(raw_sandbox_id if raw_sandbox_id is not None else self.sandbox_id),
            state=state_from_raw(raw),
            raw=raw,
        )

    def run_command(self, command: str, *, user: str = "root", timeout: int = 30) -> CommandResult:
        commands = getattr(self._sandbox, "commands", None)
        if commands is None:
            raise RuntimeError("E2B sandbox object does not expose commands")
        command_exit_exception = _import_e2b_command_exit_exception()
        # Pass cwd="/" so non-root users with /nonexistent home (e.g. nobody
        # on the e2b template) do not trip the "cwd does not exist" check
        # in envd. Mirrors the cubesandbox SDK's default in _commands.py.
        supports_user = _accepts_keyword(commands.run, "user")
        supports_cwd = _accepts_keyword(commands.run, "cwd")
        try:
            kwargs: dict[str, Any] = {"timeout": timeout}
            if supports_user:
                kwargs["user"] = user
            if supports_cwd:
                kwargs["cwd"] = "/"
            result = commands.run(command, **kwargs)
        except Exception as exc:  # E2B raises on non-zero command exits.
            if command_exit_exception is None or not isinstance(exc, command_exit_exception):
                raise
            return CommandResult(
                stdout=str(getattr(exc, "stdout", "") or ""),
                stderr=str(getattr(exc, "stderr", "") or ""),
                exit_code=int(getattr(exc, "exit_code")),
            )
        return CommandResult(
            stdout=str(getattr(result, "stdout", "") or ""),
            stderr=str(getattr(result, "stderr", "") or ""),
            exit_code=int(getattr(result, "exit_code", getattr(result, "exitCode", 0))),
        )

    def write_file(self, path: str, content: str, *, user: str = "root") -> None:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        writer = getattr(files, "write", None) or getattr(files, "write_file", None)
        if not callable(writer):
            raise RuntimeError("E2B files object does not expose write/write_file")
        writer(path, content)

    def read_file(self, path: str, *, user: str = "root") -> str:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        reader = getattr(files, "read", None) or getattr(files, "read_file", None)
        if not callable(reader):
            raise RuntimeError("E2B files object does not expose read/read_file")
        return str(reader(path))

    def _files_call(self, names: tuple[str, ...], *args):
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        method = next(
            (
                getattr(files, name, None)
                for name in names
                if callable(getattr(files, name, None))
            ),
            None,
        )
        if method is None:
            raise RuntimeError(f"E2B files object does not expose any of {names!r}")
        return method(*args)

    def list_files(self, path: str) -> list[dict[str, Any]]:
        return [_normalize_info_value(item) for item in self._files_call(("list",), path)]

    def stat_file(self, path: str) -> dict[str, Any]:
        return dict(_normalize_info_value(self._files_call(("stat", "get_info"), path)))

    def file_exists(self, path: str) -> bool:
        return bool(self._files_call(("exists",), path))

    def remove_file(self, path: str) -> None:
        self._files_call(("remove", "delete"), path)

    def rename_file(self, old_path: str, new_path: str) -> dict[str, Any]:
        value = self._files_call(("rename", "move"), old_path, new_path)
        return dict(_normalize_info_value(value) or {})

    def make_dir(self, path: str) -> None:
        self._files_call(("make_dir", "mkdir"), path)

    def write_files(self, files: list[tuple[str, str | bytes]]) -> int:
        entries = [{"path": path, "data": data} for path, data in files]
        return len(self._files_call(("write_files",), entries))

    def watch_dir_events(
        self,
        path: str,
        operation: Callable[[], None],
        *,
        timeout: float = 5,
        until: Callable[[list[dict[str, str]]], bool] | None = None,
    ) -> list[dict[str, str]]:
        events: list[dict[str, str]] = []

        def append_events(batch: Any) -> None:
            events.extend(
                {
                    "name": str(getattr(event, "name", "")),
                    "type": str(getattr(getattr(event, "type", None), "value", "")),
                }
                for event in batch
            )

        files = getattr(self._sandbox, "files", None)
        watch_dir = getattr(files, "watch_dir", None)
        if not callable(watch_dir):
            raise RuntimeError("E2B files object does not expose watch_dir()")
        watcher = watch_dir(path)
        get_new_events = getattr(watcher, "get_new_events", None)
        if not callable(get_new_events):
            raise RuntimeError(
                "E2B watch_dir() returned a watcher without get_new_events()"
            )

        deadline = time.monotonic() + timeout
        previous_count = -1
        unchanged_since = time.monotonic()
        try:
            # Give both callback- and polling-based SDK implementations time to
            # establish the remote watch before the first filesystem mutation.
            time.sleep(0.1)
            operation()
            while time.monotonic() < deadline:
                append_events(get_new_events())
                count = len(events)
                if until is not None and until(events):
                    return events
                if count != previous_count:
                    previous_count = count
                    unchanged_since = time.monotonic()
                elif (
                    until is None
                    and count
                    and time.monotonic() - unchanged_since >= 0.5
                ):
                    return events
                time.sleep(0.1)
        finally:
            stop = getattr(watcher, "stop", None)
            if callable(stop):
                stop()
        return events

    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        result = self._sandbox.run_code(code, timeout=timeout)
        logs = getattr(result, "logs", None)
        stdout = list(getattr(logs, "stdout", []) or [])
        stderr = list(getattr(logs, "stderr", []) or [])
        return CodeResult(
            text=str(getattr(result, "text", "") or ""),
            stdout=[str(item) for item in stdout],
            stderr=[str(item) for item in stderr],
            error=getattr(result, "error", None),
        )

    def get_host(self, port: int) -> str:
        get_host = getattr(self._sandbox, "get_host", None)
        if callable(get_host):
            return str(get_host(port))
        get_url = getattr(self._sandbox, "get_url", None)
        if callable(get_url):
            return str(get_url(port))
        raise RuntimeError("E2B sandbox object does not expose get_host/get_url")

    def traffic_access_token(self) -> str | None:
        token = getattr(self._sandbox, "traffic_access_token", None)
        if token:
            return str(token)
        try:
            raw = self.info().raw
        except Exception:  # noqa: BLE001 - token lookup should degrade gracefully
            return None
        token = first_present(raw, "traffic_access_token", "trafficAccessToken")
        return str(token) if token else None

    def pause(self, *, timeout: int = 60) -> None:
        pause = getattr(self._sandbox, "pause", None)
        if not callable(pause):
            raise RuntimeError("E2B sandbox object does not expose pause()")
        kwargs = {"request_timeout": timeout} if _accepts_keyword(pause, "request_timeout") else {}
        pause(**kwargs)

    def resume_or_connect(self, *, timeout: int = 60) -> "E2BAdapter":
        return type(self).connect(
            self.sandbox_id,
            self._e2e_config or SdkE2EConfig.from_env(),
            timeout=timeout,
        )

    def set_timeout(self, timeout: int) -> None:
        method = getattr(self._sandbox, "set_timeout", None)
        if not callable(method):
            raise RuntimeError("E2B sandbox object does not expose set_timeout()")
        method(timeout)

    def kill(self) -> None:
        for name in ("kill", "delete", "close"):
            method = getattr(self._sandbox, name, None)
            if callable(method):
                method()
                return
        raise RuntimeError("E2B sandbox object does not expose kill/delete/close")

    # ── filesystem metadata ops ────────────────────────────────────────────

    def _files_or_raise(self):
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        return files

    def make_dir(self, path: str, *, user: str = "root") -> None:
        files = self._files_or_raise()
        fn = getattr(files, "make_dir", None)
        if not callable(fn):
            raise RuntimeError("E2B files object does not expose make_dir")
        fn(path)

    def list_dir(self, path: str, *, user: str = "root") -> list[str]:
        files = self._files_or_raise()
        fn = getattr(files, "list", None) or getattr(files, "list_dir", None)
        if not callable(fn):
            raise RuntimeError("E2B files object does not expose list/list_dir")
        entries = fn(path)
        result: list[str] = []
        for e in entries or []:
            if isinstance(e, str):
                result.append(e)
            elif hasattr(e, "name"):
                result.append(str(e.name))
            elif isinstance(e, dict):
                result.append(str(e.get("name", e)))
        return result

    def file_exists(self, path: str, *, user: str = "root") -> bool:
        files = self._files_or_raise()
        fn = getattr(files, "exists", None)
        if not callable(fn):
            raise RuntimeError("E2B files object does not expose exists")
        return bool(fn(path))

    def remove_file(self, path: str, *, user: str = "root") -> None:
        files = self._files_or_raise()
        fn = getattr(files, "remove", None) or getattr(files, "delete", None)
        if not callable(fn):
            raise RuntimeError("E2B files object does not expose remove/delete")
        fn(path)

    def rename_file(self, old_path: str, new_path: str, *, user: str = "root") -> None:
        files = self._files_or_raise()
        fn = getattr(files, "rename", None) or getattr(files, "move", None)
        if not callable(fn):
            raise RuntimeError("E2B files object does not expose rename/move")
        fn(old_path, new_path)

    @classmethod
    def list_sandboxes(cls, config: SdkE2EConfig) -> list[dict[str, Any]]:
        Sandbox = _import_e2b_sandbox()
        list_method = getattr(Sandbox, "list", None)
        if not callable(list_method):
            raise RuntimeError("E2B sandbox SDK does not expose Sandbox.list()")
        try:
            entries = list_method(**_e2b_api_params(config))
            if hasattr(entries, "next_items"):
                items: list[Any] = []
                page_count = 0
                while getattr(entries, "has_next", False):
                    if page_count >= MAX_LIST_PAGES:
                        raise RuntimeError(
                            f"E2B sandbox listing exceeded {MAX_LIST_PAGES} pages"
                        )
                    items.extend(entries.next_items())
                    page_count += 1
                entries = items
            return [_sandbox_entry_to_dict(entry) for entry in entries or []]
        except Exception:
            # Some CubeSandbox-compatible deployments expose a list response that
            # the E2B SDK model parser cannot deserialize. Use CubeAPI directly
            # for diagnostics and cleanup assertions.
            return _list_sandboxes_via_cubeapi(config)


def _e2b_api_params(config: SdkE2EConfig) -> dict[str, Any]:
    api_key = os.environ.get("E2B_API_KEY")
    if not api_key:
        raise RuntimeError("E2B backend requires E2B_API_KEY")
    params = {
        "api_url": config.cube_api_url,
        "api_key": api_key,
        "validate_api_key": config.e2b_validate_api_key,
    }
    return {
        key: value
        for key, value in params.items()
        if _connection_config_accepts_keyword(key)
    }


def _connection_config_accepts_keyword(name: str) -> bool:
    try:
        from e2b.connection_config import ConnectionConfig  # type: ignore
    except ImportError:
        return True
    return _accepts_keyword(ConnectionConfig, name)


def _accepts_keyword(callable_obj: Any, name: str) -> bool:
    try:
        signature = inspect.signature(callable_obj)
    except (TypeError, ValueError):
        return True
    return name in signature.parameters or any(
        parameter.kind is inspect.Parameter.VAR_KEYWORD
        for parameter in signature.parameters.values()
    )
