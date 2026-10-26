#!/usr/bin/env python3
"""cube-sandbox 高并发任务运行器

在单机上以高并发度运行沙箱任务，提供实时 TUI 仪表盘展示：
  - 各实例创建耗时（avg / p50 / p95 / max + 直方图）
  - 任务执行状态与进度（Pending → Creating → Running → Done / Failed）
  - microVM 实例信息（集成 cubecli ls）

子命令:
    benchmark  压测模式 – 纯粹测试沙箱创建/销毁吞吐
    swebench   评测模式 – 高并发运行 SWE-bench RL 任务

示例:
    python scripts/run-concurrent.py benchmark -w 10 -n 20
    python scripts/run-concurrent.py benchmark -w 8 -n 50 --keep

    python scripts/run-concurrent.py swebench -w 4 \\
        -m deepseek/deepseek-chat -c configs/e2b-deepseek.yaml --slice 0:10

    python scripts/run-concurrent.py swebench -w 6 \\
        -m openai/glm-5 -c configs/e2b-tokenhub.yaml \\
        --instances django__django-13447,django__django-13710
"""

from __future__ import annotations

import argparse
import concurrent.futures
import copy
import json
import math
import os
import shutil
import subprocess
import sys
import threading
import time
import traceback
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Any, Callable

PROJECT_DIR = Path(__file__).resolve().parent.parent

# ── Rich imports ─────────────────────────────────────────────────────────

try:
    from rich.console import Console, Group
    from rich.live import Live
    from rich.panel import Panel
    from rich.table import Table
except ImportError:
    sys.exit(
        "Error: 'rich' package is required.\n"
        "  pip install rich   (or: pip install -r requirements.txt)"
    )

console = Console()


# ── Task state tracking ─────────────────────────────────────────────────


class TaskState(Enum):
    PENDING = "pending"
    CREATING = "creating"
    RUNNING = "running"
    DONE = "done"
    FAILED = "failed"


STATE_DISPLAY: dict[TaskState, tuple[str, str, str]] = {
    TaskState.PENDING: ("dim", "⏳", "Pending"),
    TaskState.CREATING: ("yellow", "⟳", "Creating"),
    TaskState.RUNNING: ("cyan", "▶", "Running"),
    TaskState.DONE: ("green", "✓", "Done"),
    TaskState.FAILED: ("red", "✗", "Failed"),
}


@dataclass
class TaskInfo:
    task_id: int
    instance_id: str = ""
    model_name: str = ""
    state: TaskState = TaskState.PENDING
    create_start: float = 0.0
    create_duration_ms: float = 0.0
    api_call_ms: float = 0.0
    sdk_init_ms: float = 0.0
    task_start: float = 0.0
    task_end: float = 0.0
    sandbox_id: str = ""
    steps: int = 0
    cost: float = 0.0
    exit_status: str = ""
    error: str = ""
    status_text: str = ""
    _create_t0: float = 0.0

    @property
    def create_elapsed_ms(self) -> float:
        """Completed duration or live elapsed since create started."""
        if self.create_duration_ms > 0:
            return self.create_duration_ms
        if self._create_t0 > 0:
            return (time.monotonic() - self._create_t0) * 1000
        return 0.0

    @property
    def total_elapsed(self) -> float:
        start = self.create_start or self.task_start
        if start <= 0:
            return 0.0
        end = self.task_end if self.task_end > 0 else time.time()
        return end - start


# ── Utilities ────────────────────────────────────────────────────────────


def fmt_dur(s: float) -> str:
    if s <= 0:
        return "-"
    if s < 60:
        return f"{s:.1f}s"
    m, sec = divmod(int(s), 60)
    if m < 60:
        return f"{m}m{sec:02d}s"
    h, m = divmod(m, 60)
    return f"{h}h{m:02d}m"


def fmt_ms(ms: float) -> str:
    """Format milliseconds."""
    if ms <= 0:
        return "-"
    return f"{round(ms)}ms"


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * p / 100.0
    lo, hi = int(math.floor(k)), int(math.ceil(k))
    if lo == hi:
        return s[lo]
    return s[lo] * (hi - k) + s[hi] * (k - lo)


def load_env() -> None:
    env_path = PROJECT_DIR / ".env"
    if env_path.exists():
        try:
            from dotenv import load_dotenv

            load_dotenv(env_path, override=False)
        except ImportError:
            pass
    ssl = os.environ.get("SSL_CERT_FILE", "")
    if ssl and not os.environ.get("CUBE_SSL_CERT_FILE"):
        os.environ["CUBE_SSL_CERT_FILE"] = ssl
        os.environ.pop("SSL_CERT_FILE", None)


def cubecli_ls() -> str:
    if not shutil.which("cubecli"):
        return ""
    try:
        r = subprocess.run(
            ["cubecli", "ls"], capture_output=True, text=True, timeout=10
        )
        return r.stdout.strip() if r.returncode == 0 else ""
    except Exception:
        return ""


# ── Dashboard ────────────────────────────────────────────────────────────


class Dashboard:
    def __init__(
        self,
        tasks: list[TaskInfo],
        mode: str,
        workers: int,
        template: str,
        model: str = "",
        show_create_detail: bool = True,
        max_rows: int = 35,
    ):
        self.tasks = tasks
        self.mode = mode
        self.workers = workers
        self.template = template
        self.model = model
        self.show_create_detail = show_create_detail
        self.max_rows = max_rows if max_rows > 0 else len(tasks)
        self.cubecli_text = ""
        self.t0 = time.time()

    def render(self) -> Group:
        return Group(
            self._header(),
            self._stats(),
            self._table(),
            self._cubecli(),
        )

    def _header(self) -> Panel:
        tpl = self.template[:24] + ("…" if len(self.template) > 24 else "")
        parts = [
            f"Mode: [bold cyan]{self.mode}[/]",
            f"Workers: [bold cyan]{self.workers}[/]",
            f"Template: [bold cyan]{tpl}[/]",
        ]
        if self.model:
            parts.append(f"Model: [bold cyan]{self.model}[/]")
        parts.append(f"Elapsed: [bold cyan]{fmt_dur(time.time() - self.t0)}[/]")
        return Panel(
            " │ ".join(parts),
            title="[bold]cube-sandbox Concurrent Runner[/]",
            border_style="bright_blue",
        )

    def _stats(self) -> Panel:
        ct = [t.api_call_ms for t in self.tasks if t.api_call_ms > 0]
        n_ok = sum(1 for t in self.tasks if t.state == TaskState.DONE)
        n_fail = sum(1 for t in self.tasks if t.state == TaskState.FAILED)
        n_run = sum(
            1
            for t in self.tasks
            if t.state in (TaskState.CREATING, TaskState.RUNNING)
        )
        n_pend = sum(1 for t in self.tasks if t.state == TaskState.PENDING)
        done = n_ok + n_fail
        total = len(self.tasks)
        ratio = done / total if total else 0

        bar_w = 30
        filled = int(bar_w * ratio)
        bar = f"[green]{'━' * filled}[/][dim]{'━' * (bar_w - filled)}[/]"
        line1 = f"{bar}  {done}/{total}  ({ratio * 100:.0f}%)"

        parts = [
            f"Pending [dim]{n_pend}[/]",
            f"Running [blue]{n_run}[/]",
            f"Done [green]{n_ok}[/]",
            f"Failed [red]{n_fail}[/]",
        ]
        if ct:
            parts.append(
                f"│ SandboxCreate  avg [cyan]{fmt_ms(sum(ct) / len(ct))}[/]"
                f"  p50 [cyan]{fmt_ms(percentile(ct, 50))}[/]"
                f"  p95 [cyan]{fmt_ms(percentile(ct, 95))}[/]"
                f"  max [cyan]{fmt_ms(max(ct))}[/]"
            )
        total_cost = sum(t.cost for t in self.tasks)
        if total_cost > 0:
            parts.append(f"│ Cost [yellow]${total_cost:.2f}[/]")

        return Panel(
            f"{line1}\n{'  '.join(parts)}",
            title="[bold]Stats[/]",
            border_style="green",
        )

    def _table(self) -> Panel:
        multi_model = len(set(t.model_name for t in self.tasks if t.model_name)) > 1
        tbl = Table(
            show_header=True,
            header_style="bold",
            expand=True,
            show_lines=False,
            pad_edge=True,
        )
        tbl.add_column("#", width=4, justify="right", style="dim")
        if multi_model:
            tbl.add_column("Model", min_width=14, max_width=24, no_wrap=True)
        tbl.add_column("Instance", min_width=16, max_width=30, no_wrap=True)
        tbl.add_column("Status", min_width=16)
        if self.show_create_detail:
            tbl.add_column("E2ECreate", width=10, justify="right")
        tbl.add_column("SandboxCreate", width=14, justify="right")
        if self.show_create_detail:
            tbl.add_column("E2BSDK", width=8, justify="right")
        tbl.add_column("Elapsed", width=8, justify="right")
        if self.mode == "swebench":
            tbl.add_column("Steps", width=6, justify="right")
            tbl.add_column("Cost", width=8, justify="right")
        tbl.add_column("Sandbox ID", width=14, no_wrap=True)

        active = [
            t
            for t in self.tasks
            if t.state in (TaskState.CREATING, TaskState.RUNNING)
        ]
        completed = [
            t for t in self.tasks if t.state in (TaskState.DONE, TaskState.FAILED)
        ]
        pending = [t for t in self.tasks if t.state == TaskState.PENDING]
        ordered = active + completed + pending

        show = ordered[: self.max_rows]
        hidden = len(ordered) - len(show)

        for t in show:
            style, icon, label = STATE_DISPLAY.get(t.state, ("", "?", "?"))
            st = t.status_text or label
            status_cell = f"[{style}]{icon} {st}[/]"
            cr = fmt_ms(t.create_elapsed_ms) if t.create_start > 0 else "-"
            if t.state == TaskState.CREATING:
                cr += "…"
            api = fmt_ms(t.api_call_ms) if t.api_call_ms > 0 else "-"
            sdk = fmt_ms(t.sdk_init_ms) if t.sdk_init_ms > 0 else "-"
            el = fmt_dur(t.total_elapsed) if t.create_start > 0 else "-"
            sid = t.sandbox_id[:12] if t.sandbox_id else "-"
            iid = (
                t.instance_id
                if len(t.instance_id) <= 28
                else t.instance_id[:25] + "…"
            )
            row: list[str] = [str(t.task_id)]
            if multi_model:
                mn = t.model_name.split("/")[-1] if t.model_name else "-"
                if len(mn) > 22:
                    mn = mn[:19] + "…"
                row.append(mn)
            row.extend([iid, status_cell])
            if self.show_create_detail:
                row.append(cr)
            row.append(api)
            if self.show_create_detail:
                row.append(sdk)
            row.append(el)
            if self.mode == "swebench":
                row.append(str(t.steps) if t.steps else "-")
                row.append(f"${t.cost:.2f}" if t.cost > 0 else "-")
            row.append(sid)
            tbl.add_row(*row)

        if hidden > 0:
            ncols = len(tbl.columns)
            tbl.add_row(f"[dim]… +{hidden} more[/]", *[""] * (ncols - 1))

        return Panel(tbl, title="[bold]Tasks[/]", border_style="yellow")

    def _cubecli(self) -> Panel:
        body = self.cubecli_text or "[dim]cubecli not available or no instances[/]"
        return Panel(
            body,
            title="[bold]microVM Instances (cubecli ls)[/]",
            border_style="magenta",
        )


# ── Shared API client (connection pooling) ───────────────────────────────

_bench_client = None
_bench_client_lock = threading.Lock()


def _get_bench_client(max_conns: int = 10):
    """
    Return a shared, thread-safe API client with HTTP connection pooling.

    The default SDK creates a new ApiClient (httpx.Client + TCP connection)
    for every _create_sandbox / kill call.  Sharing one client means the
    keep-alive pool is reused, saving per-request TCP + TLS handshake cost.
    """
    global _bench_client
    if _bench_client is not None:
        return _bench_client
    with _bench_client_lock:
        if _bench_client is not None:
            return _bench_client
        from httpx import Limits
        from e2b.api import ApiClient
        from e2b.connection_config import ConnectionConfig

        config = ConnectionConfig()
        limits = Limits(
            max_connections=max_conns + 2,
            max_keepalive_connections=max_conns + 2,
            keepalive_expiry=300,
        )
        try:
            client = ApiClient(config, limits=limits)
        except TypeError:
            client = ApiClient(config)
        client.get_httpx_client()
        _bench_client = client
        return _bench_client


# ── Benchmark task ───────────────────────────────────────────────────────


def benchmark_task(task: TaskInfo, template: str, run_cmd: str, keep: bool, max_conns: int = 10) -> None:
    from e2b_code_interpreter import Sandbox
    from e2b.connection_config import ConnectionConfig
    from e2b.api.client.types import Unset
    from e2b.api.client.models import NewSandbox as NewSandboxModel, Error
    from e2b.api.client.api.sandboxes import post_sandboxes, delete_sandboxes_sandbox_id

    api_client = _get_bench_client(max_conns)

    sandbox = None
    sandbox_id = None
    try:
        task.state = TaskState.CREATING
        task.status_text = "Creating"
        task.create_start = time.time()
        task._create_t0 = time.monotonic()

        # Phase 1: API call — shared connection pool avoids per-request TCP setup
        t1 = time.monotonic()
        res = post_sandboxes.sync_detailed(
            body=NewSandboxModel(
                template_id=template, timeout=300, auto_pause=False,
                metadata={}, env_vars={}, secure=True,
                allow_internet_access=True,
            ),
            client=api_client,
        )
        task.api_call_ms = round((time.monotonic() - t1) * 1000)

        if res.status_code >= 300 or res.parsed is None:
            msg = getattr(res.parsed, "message", "") if res.parsed else ""
            raise Exception(f"API {res.status_code}: {msg or res.content}")
        if isinstance(res.parsed, Error):
            raise Exception(f"API error: {res.parsed.message}")

        sandbox_id = res.parsed.sandbox_id
        token = res.parsed.envd_access_token
        if isinstance(token, Unset):
            token = None

        # Phase 2: SDK client init — envd httpx transport
        t2 = time.monotonic()
        extra_headers = {}
        if token:
            extra_headers["X-Access-Token"] = token
        conn = ConnectionConfig(extra_sandbox_headers=extra_headers)
        domain = res.parsed.domain
        if isinstance(domain, Unset):
            domain = None
        sandbox = Sandbox(
            sandbox_id=sandbox_id,
            sandbox_domain=domain,
            envd_version=res.parsed.envd_version,
            envd_access_token=token,
            connection_config=conn,
        )
        task.sdk_init_ms = round((time.monotonic() - t2) * 1000)

        task.create_duration_ms = task.api_call_ms + task.sdk_init_ms
        task.sandbox_id = sandbox_id
        task.state = TaskState.RUNNING
        task.task_start = time.time()

        if run_cmd:
            task.status_text = "Running cmd"
            cmd_res = sandbox.commands.run(run_cmd, user="root", timeout=60)
            if cmd_res.exit_code != 0:
                raise RuntimeError(
                    f"cmd exit {cmd_res.exit_code}: {(cmd_res.stderr or '')[:200]}"
                )

        task.state = TaskState.DONE
        task.status_text = "Done"
        task.exit_status = "success"

    except Exception as e:
        task.state = TaskState.FAILED
        task.status_text = type(e).__name__
        task.error = str(e)[:300]
        task.exit_status = type(e).__name__

    finally:
        task.task_end = time.time()
        if sandbox_id and not keep:
            try:
                # Destroy via shared client (connection reuse)
                delete_sandboxes_sandbox_id.sync_detailed(
                    sandbox_id, client=api_client,
                )
            except Exception:
                pass


# ── SWE-bench task ───────────────────────────────────────────────────────

_preds_lock = threading.Lock()


def _update_preds(path: Path, instance_id: str, model_name: str, patch: str) -> None:
    with _preds_lock:
        data = json.loads(path.read_text()) if path.exists() else {}
        data[instance_id] = {
            "model_name_or_path": model_name,
            "instance_id": instance_id,
            "model_patch": patch,
        }
        path.write_text(json.dumps(data, indent=2))


def _swebench_image(instance: dict) -> str:
    name = instance.get("image_name") or instance.get("docker_image")
    if not name:
        iid = instance["instance_id"].replace("__", "_1776_")
        name = f"docker.io/swebench/sweb.eval.x86_64.{iid}:latest".lower()
    return name


def _load_e2b_class():
    """Import E2BEnvironment, falling back to project patch directory."""
    try:
        from minisweagent.environments.extra.e2b import E2BEnvironment

        return E2BEnvironment
    except (ImportError, AttributeError):
        pass
    patch_e2b = PROJECT_DIR / "mini-swe-agent-patch" / "environments" / "extra" / "e2b.py"
    if not patch_e2b.exists():
        raise ImportError(
            "E2BEnvironment not found. Run: bash mini-swe-agent-patch/install.sh"
        )
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "minisweagent.environments.extra.e2b", str(patch_e2b)
    )
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod
    spec.loader.exec_module(mod)
    return mod.E2BEnvironment


def swebench_task(
    task: TaskInfo,
    instance: dict,
    config: dict,
    output_dir: Path,
    e2b_cls: type | None = None,
    template_id: str = "",
    repeat_idx: int = -1,
    sandbox_info: Any = None,
    sandbox_only: bool = False,
) -> None:
    E2BEnvironment = e2b_cls or _load_e2b_class()

    cfg = copy.deepcopy(config)
    instance_id = instance["instance_id"]
    run_suffix = f"_run{repeat_idx + 1}" if repeat_idx >= 0 else ""
    inst_dir = output_dir / f"{instance_id}{run_suffix}"
    inst_dir.mkdir(parents=True, exist_ok=True)

    env = None
    agent = None
    result = ""

    try:
        # Phase 1: Create or connect environment
        task.state = TaskState.CREATING
        task.create_start = time.time()
        task._create_t0 = time.monotonic()

        env_cfg = cfg.get("environment", {})
        env_cfg.pop("environment_class", None)
        image = _swebench_image(instance)
        env_cfg["image"] = image
        if template_id:
            env_cfg["template_id"] = template_id

        if sandbox_info is not None:
            task.status_text = "Connecting"
            env = E2BEnvironment(sandbox_info=sandbox_info, **env_cfg)
        else:
            task.status_text = "Creating sandbox"
            env = E2BEnvironment(**env_cfg)

        task.api_call_ms = getattr(env, "api_call_ms", 0)
        task.sdk_init_ms = getattr(env, "sdk_init_ms", 0)
        task.create_duration_ms = getattr(env, "setup_time_ms", 0) or round((time.monotonic() - task._create_t0) * 1000)
        if hasattr(env, "sandbox"):
            task.sandbox_id = env.sandbox.sandbox_id

        task.state = TaskState.RUNNING
        task.task_start = time.time()

        # Startup command
        if startup := cfg.get("run", {}).get("env_startup_command"):
            from jinja2 import StrictUndefined, Template

            cmd = Template(startup, undefined=StrictUndefined).render(**instance)
            out = env.execute({"command": cmd})
            if out["returncode"] != 0:
                raise RuntimeError(f"Startup command failed: {out}")

        if sandbox_only:
            task.state = TaskState.DONE
            task.status_text = "Done (sandbox-only)"
            task.exit_status = "sandbox_only"
            return

        # Phase 2: Run agent with step tracking
        from minisweagent.agents.default import DefaultAgent
        from minisweagent.models import get_model

        model = get_model(config=cfg.get("model", {}))
        agent = DefaultAgent(model, env, **cfg.get("agent", {}))

        _original_step = agent.step

        def tracked_step():
            task.status_text = f"Step {agent.n_calls + 1}"
            ret = _original_step()
            task.steps = agent.n_calls
            task.cost = agent.cost
            task.status_text = f"Step {agent.n_calls} (${agent.cost:.2f})"
            return ret

        agent.step = tracked_step

        info = agent.run(instance["problem_statement"])
        task.exit_status = info.get("exit_status", "unknown")
        result = info.get("submission", "")

        traj_path = inst_dir / f"{instance_id}.traj.json"
        agent.save(
            traj_path,
            {
                "info": {
                    "exit_status": task.exit_status,
                    "submission": result,
                    "create_duration_ms": task.create_duration_ms,
                },
                "instance_id": instance_id,
            },
        )

        task.state = TaskState.DONE
        task.status_text = f"Done ({task.exit_status})"

    except Exception as e:
        task.state = TaskState.FAILED
        task.status_text = type(e).__name__
        task.error = str(e)[:300]
        task.exit_status = type(e).__name__

    finally:
        task.task_end = time.time()
        if agent:
            task.steps = agent.n_calls
            task.cost = agent.cost
        if env and hasattr(env, "cleanup"):
            env.cleanup()
        model_name = cfg.get("model", {}).get("model_name", "unknown")
        pred_key = f"{instance_id}{run_suffix}"
        _update_preds(output_dir / "preds.json", pred_key, model_name, result)


# ── Concurrent runner ────────────────────────────────────────────────────


def run_concurrent(
    tasks: list[TaskInfo],
    dashboard: Dashboard,
    task_fn: Callable,
    task_args: list[tuple],
    workers: int,
    cubecli_interval: float,
) -> None:
    stop = threading.Event()
    has_cubecli = shutil.which("cubecli") is not None

    def cubecli_loop():
        while not stop.is_set():
            dashboard.cubecli_text = cubecli_ls()
            stop.wait(cubecli_interval)

    if has_cubecli and cubecli_interval > 0:
        threading.Thread(target=cubecli_loop, daemon=True).start()

    with Live(dashboard.render(), refresh_per_second=4, console=console) as live:
        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
            futures = {
                pool.submit(task_fn, tasks[i], *task_args[i]): i
                for i in range(len(tasks))
            }
            completed: set[concurrent.futures.Future] = set()
            try:
                while len(completed) < len(futures):
                    time.sleep(0.25)
                    for f in list(futures):
                        if f.done() and f not in completed:
                            completed.add(f)
                            try:
                                f.result()
                            except Exception as e:
                                idx = futures[f]
                                t = tasks[idx]
                                if t.state not in (TaskState.DONE, TaskState.FAILED):
                                    t.state = TaskState.FAILED
                                    t.error = str(e)[:300]
                                    t.task_end = time.time()
                    live.update(dashboard.render())

            except KeyboardInterrupt:
                console.print(
                    "\n[bold yellow]Interrupted – cancelling pending tasks…[/]"
                )
                for f in futures:
                    if not f.running() and not f.done():
                        f.cancel()
                concurrent.futures.wait(futures, timeout=30)
                live.update(dashboard.render())

    stop.set()
    if has_cubecli:
        dashboard.cubecli_text = cubecli_ls()


# ── Summary ──────────────────────────────────────────────────────────────


def print_summary(tasks: list[TaskInfo], dashboard: Dashboard) -> None:
    console.print()
    console.rule("[bold]Run Summary[/]")
    console.print()

    n_ok = sum(1 for t in tasks if t.state == TaskState.DONE)
    n_fail = sum(1 for t in tasks if t.state == TaskState.FAILED)
    total = len(tasks)
    elapsed = time.time() - dashboard.t0
    total_cost = sum(t.cost for t in tasks)

    info_tbl = Table(show_header=False, box=None, padding=(0, 2))
    info_tbl.add_column(style="bold")
    info_tbl.add_column()
    info_tbl.add_row("Total Tasks", str(total))
    info_tbl.add_row("Succeeded", f"[green]{n_ok}[/]")
    info_tbl.add_row("Failed", f"[red]{n_fail}[/]")
    info_tbl.add_row("Workers", str(dashboard.workers))
    info_tbl.add_row("Total Time", fmt_dur(elapsed))
    if total_cost > 0:
        info_tbl.add_row("Total Cost", f"${total_cost:.2f}")
    console.print(info_tbl)

    # SandboxCreate time distribution
    ct = sorted(t.api_call_ms for t in tasks if t.api_call_ms > 0)
    if ct:
        console.print()
        console.print("[bold]SandboxCreate Time Distribution[/]")
        st = Table(show_header=False, box=None, padding=(0, 2))
        st.add_column(style="bold")
        st.add_column()
        st.add_row("Min", fmt_ms(min(ct)))
        st.add_row("Avg", fmt_ms(sum(ct) / len(ct)))
        st.add_row("P50", fmt_ms(percentile(ct, 50)))
        st.add_row("P95", fmt_ms(percentile(ct, 95)))
        st.add_row("Max", fmt_ms(max(ct)))
        console.print(st)

        if len(ct) > 1:
            console.print()
            max_val = max(ct)
            bin_size = max(0.1, math.ceil(max_val * 10 / 8) / 10)
            bins: list[tuple[float, float, int]] = []
            lo = 0.0
            while lo < max_val + bin_size:
                hi = lo + bin_size
                count = sum(1 for v in ct if lo <= v < hi)
                if count > 0 or lo < max_val:
                    bins.append((lo, hi, count))
                lo = hi
            bins = [(a, b, c) for a, b, c in bins if c > 0 or a <= max_val]
            max_count = max((c for _, _, c in bins), default=1) or 1
            bar_max = 30
            for lo_b, hi_b, cnt in bins:
                bar_len = int(cnt / max_count * bar_max)
                label = f"  {fmt_ms(lo_b):>8s} - {fmt_ms(hi_b):<8s}"
                bar = "█" * bar_len
                console.print(f"{label}  [cyan]{bar}[/] ({cnt})")

    # Failed tasks
    failed = [t for t in tasks if t.state == TaskState.FAILED]
    if failed:
        console.print()
        console.print("[bold red]Failed Tasks[/]")
        for t in failed:
            err = t.error[:80] if t.error else "unknown"
            console.print(f"  #{t.task_id}  {t.instance_id}  [red]{err}[/]")

    # Final cubecli snapshot
    if dashboard.cubecli_text:
        console.print()
        console.print(
            Panel(
                dashboard.cubecli_text,
                title="[bold]microVM Instances (cubecli ls)[/]",
                border_style="magenta",
            )
        )

    console.print()


# ── CLI: benchmark ───────────────────────────────────────────────────────

DATASET_MAPPING = {
    "full": "princeton-nlp/SWE-Bench",
    "verified": "princeton-nlp/SWE-Bench_Verified",
    "lite": "princeton-nlp/SWE-Bench_Lite",
    "multimodal": "princeton-nlp/SWE-Bench_Multimodal",
    "multilingual": "swe-bench/SWE-Bench_Multilingual",
}


def cmd_benchmark(args: argparse.Namespace) -> None:
    load_env()
    template = args.template or os.environ.get("CUBE_TEMPLATE_ID", "")
    if not template:
        sys.exit("Error: --template or CUBE_TEMPLATE_ID (in .env) is required.")

    # For benchmark mode, set SSL_CERT_FILE globally (only E2B calls, no LLM)
    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE", "")
    if cube_ssl:
        os.environ["SSL_CERT_FILE"] = cube_ssl

    # Pre-init shared API client for connection reuse across workers
    _get_bench_client(args.workers)

    tasks = [
        TaskInfo(task_id=i + 1, instance_id=f"sandbox-{i + 1}")
        for i in range(args.count)
    ]
    dashboard = Dashboard(tasks, "benchmark", args.workers, template,
                          max_rows=getattr(args, "max_rows", 35))
    task_args = [(template, args.run_cmd, args.keep, args.workers)] * len(tasks)

    console.print(
        f"[bold]Benchmark: creating {args.count} sandboxes"
        f" with {args.workers} workers[/]\n"
    )
    run_concurrent(
        tasks, dashboard, benchmark_task, task_args, args.workers, args.cubecli_interval
    )
    print_summary(tasks, dashboard)


# ── CLI: swebench ────────────────────────────────────────────────────────


# TokenHub model → config mapping (used by --models tokenhub)
TOKENHUB_MODELS: dict[str, str] = {
    "openai/glm-5": "e2b-tokenhub.yaml",
    "openai/glm-5-turbo": "e2b-tokenhub.yaml",
    "openai/minimax-m2.7": "e2b-tokenhub.yaml",
    "openai/deepseek-v3.2": "e2b-tokenhub.yaml",
    "openai/kimi-k2.5": "e2b-kimi.yaml",
    "openai/deepseek-r1-0528": "e2b-kimi.yaml",
    "openai/hunyuan-2.0-thinking-20251109": "e2b-kimi.yaml",
}


def _resolve_models(model_arg: str, config_arg: str) -> list[tuple[str, str]]:
    """Resolve model spec into list of (model_name, config_path).

    Supports:
        -m openai/glm-5 -c configs/e2b-tokenhub.yaml   → single model
        -m openai/glm-5,openai/kimi-k2.5 -c ...        → multi models, same config
        -m tokenhub                                     → all TokenHub models
    """
    configs_dir = PROJECT_DIR / "configs"

    if model_arg.lower() == "tokenhub":
        return [
            (name, str(configs_dir / cfg))
            for name, cfg in TOKENHUB_MODELS.items()
        ]

    models = [m.strip() for m in model_arg.split(",") if m.strip()]
    if not models:
        return []

    result = []
    for m in models:
        if m in TOKENHUB_MODELS and not config_arg:
            cfg = str(configs_dir / TOKENHUB_MODELS[m])
        else:
            cfg = config_arg
        result.append((m, cfg))
    return result


def cmd_swebench(args: argparse.Namespace) -> None:
    load_env()

    if not args.model:
        sys.exit("Error: --model / -m is required.")

    # Resolve models — may be single, comma-separated, or 'tokenhub'
    model_configs = _resolve_models(args.model, args.config)
    if not model_configs:
        sys.exit("Error: no models resolved.")

    # Validate: if not using a preset, config is required
    for m, c in model_configs:
        if not c:
            sys.exit(f"Error: --config / -c is required for model '{m}'.")

    multi_model = len(model_configs) > 1
    if multi_model:
        console.print(f"Models ({len(model_configs)}):")
        for m, c in model_configs:
            console.print(f"  [cyan]{m}[/]  config=[dim]{Path(c).name}[/]")
        console.print()

    default_template = os.environ.get("CUBE_TEMPLATE_ID", "")

    # Load template mapping (optional)
    template_map: dict[str, str] = {}
    if args.template_map:
        map_path = Path(args.template_map)
        if not map_path.exists():
            sys.exit(f"Error: template map not found: {map_path}")
        raw = json.loads(map_path.read_text())
        template_map = {k: v for k, v in raw.items() if not k.startswith("_")}
        console.print(
            f"Loaded template mapping: [cyan]{len(template_map)}[/] entries"
        )

    # Build per-model configs
    from minisweagent.config import get_config_from_spec
    from minisweagent.utils.serialize import UNSET, recursive_merge

    per_model_configs: list[tuple[str, dict]] = []
    for model_name, config_path in model_configs:
        config_specs: list[str] = [config_path]
        if args.step_limit:
            config_specs.append(f"agent.step_limit={args.step_limit}")
        configs = [get_config_from_spec(s) for s in config_specs]
        configs.append(
            {
                "environment": {"environment_class": "e2b"},
                "model": {"model_name": model_name},
            }
        )
        per_model_configs.append((model_name, recursive_merge(*configs)))

    # Load dataset
    from datasets import load_dataset

    ds_path = DATASET_MAPPING.get(args.subset, args.subset)
    console.print(f"Loading dataset [cyan]{ds_path}[/] split=[cyan]{args.split}[/]…")
    instances = list(load_dataset(ds_path, split=args.split))

    if args.instances:
        ids = set(args.instances.split(","))
        instances = [i for i in instances if i["instance_id"] in ids]
    if args.filter:
        import re

        instances = [
            i for i in instances if re.match(args.filter, i["instance_id"])
        ]
    if args.slice:
        vals = [int(x) if x else None for x in args.slice.split(":")]
        instances = instances[slice(*vals)]

    if not instances:
        sys.exit("No instances match the given filters.")

    # Expand: model × instance × repeat
    repeat = max(1, args.repeat)
    sandbox_only = getattr(args, "sandbox_only", False)

    # Each entry: (model_name, config, instance, repeat_idx)
    expanded: list[tuple[str, dict, dict, int]] = []
    for model_name, cfg in per_model_configs:
        for r in range(repeat):
            for inst in instances:
                expanded.append((model_name, cfg, inst, r))

    total_tasks = len(expanded)
    n_models = len(per_model_configs)
    n_inst = len(instances)
    desc_parts = [f"[bold]{n_inst}[/] instance(s)"]
    if n_models > 1:
        desc_parts.append(f"[bold]{n_models}[/] model(s)")
    if repeat > 1:
        desc_parts.append(f"[bold]{repeat}[/] repeat")
    console.print(f"Task matrix: {' × '.join(desc_parts)} = [bold]{total_tasks}[/] tasks\n")

    # Output directory
    ts = time.strftime("%Y%m%d-%H%M%S")
    if multi_model:
        dir_label = f"multi-{n_models}models"
    else:
        dir_label = model_configs[0][0].split("/")[-1]
    output_dir = Path(args.output or f"results/concurrent-{dir_label}-{ts}")
    output_dir.mkdir(parents=True, exist_ok=True)

    # Suppress mini-swe-agent console logs (they interfere with the TUI)
    import logging

    msa_logger = logging.getLogger("minisweagent")
    msa_logger.setLevel(logging.WARNING)
    fh = logging.FileHandler(output_dir / "runner.log")
    fh.setLevel(logging.DEBUG)
    fh.setFormatter(
        logging.Formatter("%(asctime)s %(name)s %(levelname)s %(message)s")
    )
    msa_logger.addHandler(fh)

    # Pre-import E2BEnvironment in main thread (signal handlers require it)
    e2b_cls = _load_e2b_class()

    # ── Pre-create sandboxes if requested ────────────────────────────────
    pre_create_workers = getattr(args, "pre_create_workers", 0) or args.workers
    sandbox_infos: list[Any] = [None] * total_tasks
    if getattr(args, "pre_create", False):
        from minisweagent.environments.extra.e2b import (
            batch_create_sandboxes,
            SandboxInfo,
        )

        # All tasks use the same template (different models, same sandbox)
        sandbox_timeout = per_model_configs[0][1].get("environment", {}).get(
            "sandbox_timeout", 1800
        )

        console.print(
            f"[bold yellow]Pre-creating {total_tasks} sandboxes"
            f" ({pre_create_workers} workers)…[/]\n"
        )

        pre_t0 = time.monotonic()
        created_count = 0

        def _on_created(idx, info):
            nonlocal created_count
            created_count += 1
            console.print(
                f"  [green]✓[/] Sandbox #{idx + 1}: {info.sandbox_id}"
                f" ({info.api_call_ms}ms)"
                f"  [{created_count}/{total_tasks}]"
            )

        # Group by template
        templates_per_task: list[str] = []
        for model_name, cfg, inst, r in expanded:
            iid = inst["instance_id"]
            templates_per_task.append(template_map.get(iid, "") or default_template)

        unique_templates = set(templates_per_task)
        for tpl in unique_templates:
            indices = [i for i, t in enumerate(templates_per_task) if t == tpl]
            results = batch_create_sandboxes(
                template=tpl,
                count=len(indices),
                timeout=sandbox_timeout,
                workers=pre_create_workers,
                on_created=_on_created,
            )
            for slot, info in zip(indices, results):
                sandbox_infos[slot] = info

        pre_ms = round((time.monotonic() - pre_t0) * 1000)
        ok_count = sum(1 for s in sandbox_infos if s is not None)
        console.print(
            f"\n[bold]Pre-created {ok_count}/{total_tasks} sandboxes"
            f" in {fmt_ms(pre_ms)}[/]\n"
        )

        if ok_count < total_tasks:
            console.print(
                f"[yellow]Warning: {total_tasks - ok_count} sandboxes"
                f" failed to create[/]"
            )

    # ── Build task list ──────────────────────────────────────────────────
    tasks = []
    task_args = []
    for i, (model_name, cfg, inst, r) in enumerate(expanded):
        iid = inst["instance_id"]
        suffix = f" #{r + 1}" if repeat > 1 else ""
        model_short = model_name.split("/")[-1]

        # Per-model output subdirectory
        if multi_model:
            task_output = output_dir / model_short
        else:
            task_output = output_dir
        task_output.mkdir(parents=True, exist_ok=True)

        tasks.append(TaskInfo(
            task_id=i + 1,
            instance_id=f"{iid}{suffix}",
            model_name=model_name,
        ))

        tpl = template_map.get(iid, "") or default_template
        ridx = r if repeat > 1 else -1
        si = sandbox_infos[i]

        if si is not None:
            tasks[-1].sandbox_id = si.sandbox_id
            tasks[-1].api_call_ms = si.api_call_ms

        task_args.append((inst, cfg, task_output, e2b_cls, tpl, ridx, si, sandbox_only))

    model_label = (
        f"{n_models} models" if multi_model else model_configs[0][0]
    )
    show_detail = not getattr(args, "no_create_detail", False)
    dashboard = Dashboard(tasks, "swebench", args.workers, default_template, model_label, show_detail,
                          max_rows=getattr(args, "max_rows", 35))

    mode_suffix = ""
    if sandbox_only:
        mode_suffix = ", sandbox-only"
    mode_label = "pre-created" if getattr(args, "pre_create", False) else "on-demand"
    console.print(
        f"[bold]SWE-bench: running {total_tasks} tasks"
        f" with {args.workers} workers ({mode_label}{mode_suffix})[/]\n"
    )
    run_concurrent(
        tasks,
        dashboard,
        swebench_task,
        task_args,
        args.workers,
        args.cubecli_interval,
    )
    print_summary(tasks, dashboard)
    console.print(f"Results: [cyan]{output_dir}[/]")


# ── Main ─────────────────────────────────────────────────────────────────


def main() -> None:
    parser = argparse.ArgumentParser(
        description="cube-sandbox 高并发任务运行器",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    sub = parser.add_subparsers(dest="command")
    sub.required = True

    # benchmark
    bp = sub.add_parser(
        "benchmark",
        help="压测模式 – 测试沙箱创建/销毁性能",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    bp.add_argument(
        "-w", "--workers", type=int, default=4, help="并发数 (default: 4)"
    )
    bp.add_argument(
        "-n", "--count", type=int, default=10, help="创建沙箱数量 (default: 10)"
    )
    bp.add_argument("--template", default="", help="模板 ID（默认从 .env 读取）")
    bp.add_argument(
        "--run-cmd",
        default="echo ok",
        help="在沙箱中执行的命令 (default: 'echo ok')",
    )
    bp.add_argument("--keep", action="store_true", help="不销毁沙箱")
    bp.add_argument(
        "--cubecli-interval",
        type=float,
        default=10,
        help="cubecli ls 刷新间隔秒数，0 禁用 (default: 10)",
    )
    bp.add_argument(
        "--max-rows", type=int, default=35, dest="max_rows",
        help="TUI 表格最大行数，0 显示全部 (default: 35)",
    )
    bp.set_defaults(func=cmd_benchmark)

    # swebench
    sp = sub.add_parser(
        "swebench",
        help="评测模式 – 高并发运行 SWE-bench 任务",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    sp.add_argument(
        "-w", "--workers", type=int, default=4, help="Agent 并发数 (default: 4)"
    )
    sp.add_argument(
        "-m", "--model", default="",
        help="模型名，支持: 单个模型, 逗号分隔多模型, 或 'tokenhub' 表示所有 TokenHub 模型",
    )
    sp.add_argument("-c", "--config", default="", help="YAML 配置文件（单模型时必填，tokenhub 自动选择）")
    sp.add_argument(
        "--subset", default="lite", help="数据集子集 (default: lite)"
    )
    sp.add_argument(
        "--split", default="test", help="数据集分割 (default: test)"
    )
    sp.add_argument("--slice", default="", help="实例切片，如 0:5")
    sp.add_argument("--filter", default="", help="实例 ID 正则过滤")
    sp.add_argument("--instances", default="", help="指定实例 ID（逗号分隔）")
    sp.add_argument(
        "--repeat",
        type=int,
        default=1,
        help="每个实例重复运行次数，用于并发压测 (default: 1)",
    )
    sp.add_argument(
        "--template-map",
        default="",
        help="模板映射 JSON 文件（instance_id → template_id）",
    )
    sp.add_argument("--step-limit", default="", help="最大 Agent 步数")
    sp.add_argument(
        "--pre-create",
        action="store_true",
        dest="pre_create",
        help="预创建所有沙箱后再启动 Agent（并行创建，减少总耗时）",
    )
    sp.add_argument(
        "--pre-create-workers",
        type=int,
        default=0,
        dest="pre_create_workers",
        help="预创建阶段并发数（默认等于 -w，可设更大值加速批量创建）",
    )
    sp.add_argument(
        "--no-create-detail",
        action="store_true",
        dest="no_create_detail",
        help="隐藏 TUI 中的 E2ECreate 和 E2BSDK 列，只显示 SandboxCreate",
    )
    sp.add_argument(
        "--sandbox-only",
        action="store_true",
        dest="sandbox_only",
        help="仅创建/销毁沙箱，跳过 LLM 调用（纯沙箱性能压测）",
    )
    sp.add_argument("-o", "--output", default="", help="输出目录")
    sp.add_argument(
        "--cubecli-interval",
        type=float,
        default=10,
        help="cubecli ls 刷新间隔秒数，0 禁用 (default: 10)",
    )
    sp.add_argument(
        "--max-rows", type=int, default=35, dest="max_rows",
        help="TUI 表格最大行数，0 显示全部 (default: 35)",
    )
    sp.set_defaults(func=cmd_swebench)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
