import os
import sys
import time

import requests
from dotenv import load_dotenv
from e2b import Sandbox


FASTIFY_PORT = 3000


def require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is not set. Copy .env.example to .env and fill it in.")
    return value


def web_url(sandbox: Sandbox, port: int = FASTIFY_PORT) -> str:
    return f"http://{sandbox.get_host(port)}"


def wait_for_health(base_url: str, timeout: float = 60.0) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            response = requests.get(f"{base_url}/health", timeout=3)
            if response.status_code == 200 and response.json().get("ok") is True:
                return
            last_error = RuntimeError(f"unexpected status {response.status_code}: {response.text}")
        except Exception as error:  # noqa: BLE001 - surfaced in the final error message.
            last_error = error
        time.sleep(1)
    raise TimeoutError(f"Fastify service was not ready at {base_url}/health: {last_error}")


def increment_counter(base_url: str) -> dict:
    response = requests.post(f"{base_url}/api/counter", timeout=10)
    response.raise_for_status()
    return response.json()


def main() -> int:
    load_dotenv()

    template_id = require_env("CUBE_TEMPLATE_ID")
    require_env("E2B_API_URL")
    require_env("E2B_API_KEY")

    sandbox = Sandbox.create(template=template_id, timeout=600)
    try:
        base_url = web_url(sandbox)
        print(f"[sandbox] created id={sandbox.sandbox_id}")
        wait_for_health(base_url)

        before = increment_counter(base_url)
        print(f"[before pause] {before}")

        sandbox.pause()
        print("[sandbox] paused")
        time.sleep(2)

        sandbox = sandbox.connect()
        base_url = web_url(sandbox)
        print(f"[sandbox] resumed id={sandbox.sandbox_id}")

        wait_for_health(base_url)
        after = increment_counter(base_url)
        print(f"[after resume] {after}")
    finally:
        sandbox.kill()

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # noqa: BLE001 - keep CLI output concise for demos.
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
