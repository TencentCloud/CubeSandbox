"""Reserve the interpreter port before starting services or Jupyter kernels."""
import os
from contextlib import ExitStack
from pathlib import Path
import signal
import shlex
import socket
import subprocess
import sys

import uvicorn


def exit_on_signal(signum, frame):
    # Uvicorn restores and re-raises captured signals. Exit through Python so
    # the launcher can reap children even when it is the container's PID 1.
    raise SystemExit(128 + signum)


def stop_child(process):
    if process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def stop_children(children):
    pending_error = sys.exc_info()[0] is not None
    first_error = None
    for child in reversed(children):
        try:
            stop_child(child)
        except (OSError, subprocess.TimeoutExpired) as error:
            print(f"[lightweight-code-interpreter] child cleanup failed: {error}", file=sys.stderr, flush=True)
            if first_error is None:
                first_error = error
    if first_error is not None and not pending_error:
        raise first_error


def main():
    host = os.getenv("CODE_INTERPRETER_HOST", "0.0.0.0")
    port = int(os.getenv("CODE_INTERPRETER_PORT", "49999"))
    workdir = Path(os.getenv("CODE_INTERPRETER_WORKDIR", "/workspace"))
    jupyter_host = os.getenv("JUPYTER_HOST", "127.0.0.1")
    jupyter_port = os.getenv("JUPYTER_PORT", "8888")
    envd_port = os.getenv("ENVD_PORT", "49983")
    url_host = f"[{jupyter_host}]" if ":" in jupyter_host else jupyter_host
    os.environ["JUPYTER_BASE_URL"] = f"http://{url_host}:{jupyter_port}"
    workdir.mkdir(parents=True, exist_ok=True)
    config = uvicorn.Config(
        "server:app", host=host, port=port, log_level="info", timeout_keep_alive=640
    )
    family = socket.AF_INET6 if ":" in host else socket.AF_INET
    children = []
    # Uvicorn's usual single-worker path binds after the application lifespan.
    # Its lifespan creates a Jupyter kernel, whose dynamic ports (or an HTTP
    # client's ephemeral port) may otherwise take the still-unbound service port.
    with socket.socket(family, socket.SOCK_STREAM) as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind((host, port))
        listener.listen(socket.SOMAXCONN)
        listener.set_inheritable(False)
        with ExitStack() as stack:
            envd_name = os.getenv("ENVD_LOG_FILE", "/var/log/envd.log")
            jupyter_name = os.getenv("JUPYTER_LOG_FILE", "/var/log/jupyter.log")
            envd_log = None if envd_name == "-" else stack.enter_context(open(envd_name, "ab"))
            jupyter_log = None if jupyter_name == "-" else stack.enter_context(open(jupyter_name, "ab"))
            previous_term = signal.signal(signal.SIGTERM, exit_on_signal)
            try:
                children.append(subprocess.Popen(
                    [os.getenv("ENVD_BIN", "/usr/bin/envd"), "-isnotfc", "-port", envd_port]
                    + shlex.split(os.getenv("ENVD_EXTRA_ARGS", "")),
                    stdin=subprocess.DEVNULL, stdout=envd_log,
                    stderr=subprocess.STDOUT, close_fds=True,
                ))
                print(f"[lightweight-code-interpreter] envd launched on :{envd_port}", flush=True)
                children.append(subprocess.Popen(
                    ["jupyter", "server", "--IdentityProvider.token=",
                     f"--ServerApp.ip={jupyter_host}", f"--ServerApp.port={jupyter_port}",
                     "--ServerApp.port_retries=0",
                     f"--ServerApp.root_dir={workdir}", "--ServerApp.allow_root=True",
                     "--ServerApp.disable_check_xsrf=True", "--ServerApp.open_browser=False"],
                    stdin=subprocess.DEVNULL, stdout=jupyter_log,
                    stderr=subprocess.STDOUT, close_fds=True,
                ))
                print(f"[lightweight-code-interpreter] Jupyter Server started on {os.environ['JUPYTER_BASE_URL']}", flush=True)
                server = uvicorn.Server(config)
                server.run(sockets=[listener])
                if not server.started:
                    raise SystemExit(3)
            finally:
                try:
                    stop_children(children)
                finally:
                    signal.signal(signal.SIGTERM, previous_term)


if __name__ == "__main__":
    main()
