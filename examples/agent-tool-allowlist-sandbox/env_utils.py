from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return
