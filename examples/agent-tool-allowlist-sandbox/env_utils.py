from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            # str() keeps older python-dotenv working; requirements pin >=0.17.
            load_dotenv(dotenv_path=str(path), override=False)
            return
