from __future__ import annotations

import os

from dotenv import load_dotenv
from dev_sidecar import setup_dev_sidecar


def main() -> None:
    load_dotenv()
    setup_dev_sidecar()

    from e2b_code_interpreter import Sandbox

    template = os.environ["CUBE_TEMPLATE_ID"]

    with Sandbox.create(template=template) as sandbox:
        execution = sandbox.run_code(
            "'Hello world Cube！'\n"
        )
        print(execution.text)


if __name__ == "__main__":
    main()
