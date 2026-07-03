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


def main() -> int:
    load_dotenv()

    template_id = require_env("CUBE_TEMPLATE_ID")
    require_env("E2B_API_URL")
    require_env("E2B_API_KEY")

    sandbox = Sandbox.create(template=template_id, timeout=600)
    try:
        base_url = web_url(sandbox)
        print(f"[sandbox] id={sandbox.sandbox_id}")
        print(f"[web] {base_url}")

        wait_for_health(base_url)

        landing = requests.get(base_url, timeout=10)
        landing.raise_for_status()
        print("\n[landing page snippet]")
        print(landing.text[:240].replace("\n", " "))

        info = requests.get(f"{base_url}/api/info", timeout=10)
        info.raise_for_status()
        print("\n[/api/info]")
        print(info.json())

        print("\n[/api/counter]")
        for _ in range(2):
            counter = requests.post(f"{base_url}/api/counter", timeout=10)
            counter.raise_for_status()
            print(counter.json())

        note = requests.post(
            f"{base_url}/api/write-note",
            json={"note": "hello from CubeSandbox Fastify demo"},
            timeout=10,
        )
        note.raise_for_status()
        print("\n[/api/write-note]")
        print(note.json())
    finally:
        sandbox.kill()

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # noqa: BLE001 - keep CLI output concise for demos.
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
