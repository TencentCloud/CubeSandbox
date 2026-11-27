#!/usr/bin/env python3
"""Package a supplied Linux VMM and a static guest into a new POC directory."""
import argparse
import pathlib
import shutil
import subprocess


def entry(name, mode, data):
    fields = [1, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name) + 1, 0]
    encoded = b"070701" + b"".join(("%08x" % x).encode() for x in fields) + name.encode() + b"\0"
    encoded += b"\0" * (-len(encoded) % 4)
    encoded += data
    return encoded + b"\0" * (-len(data) % 4)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--vmm", type=pathlib.Path, required=True)
    p.add_argument("--out", type=pathlib.Path, required=True)
    args = p.parse_args()
    args.out.mkdir()
    guest = args.out / "init"
    subprocess.run(["cc", "-static", "-O2", "-Wall", "-Wextra", "-Werror", "-o", str(guest),
                    str(pathlib.Path(__file__).with_name("poc_init.c"))], check=True)
    archive = entry("init", 0o100755, guest.read_bytes()) + entry("TRAILER!!!", 0, b"")
    (args.out / "poc-initramfs.cpio").write_bytes(archive)
    shutil.copy2(args.vmm, args.out / "poc-vmm")
    for name in ("run_poc.py", "verify_poc.py"):
        shutil.copy2(pathlib.Path(__file__).with_name(name), args.out / name)


if __name__ == "__main__":
    main()
