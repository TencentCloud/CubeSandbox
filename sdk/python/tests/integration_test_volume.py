#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end integration test for the Python SDK Volume API against a live
# CubeAPI + CubeMaster + Cubelet stack with a volume plugin configured.
#
# It exercises the full lifecycle:
#   1. Volume.create / list / get
#   2. Sandbox A: create(volume_mounts=[...]) + write a probe file
#   3. kill A, then Sandbox B: remount the SAME volume + read the file back
#      -> real cross-sandbox persistence check (proves the bytes hit the
#         backing store, e.g. cos, instead of A's local overlay)
#   4. cleanup: kill sandboxes, Volume.delete
#
# Unlike test_volume.py (pure unit tests, fully mocked), this script talks to a
# real deployment and needs env vars set.
#
# RECOMMENDED: run it ON the CubeProxy host itself (e.g. 9.135.78.206) so the
# data-plane request (*.cube.app -> CUBE_PROXY_NODE_IP:80) stays on loopback and
# bypasses the corporate security gateway that 403s remote :80 traffic:
#
#   export CUBE_API_URL=http://127.0.0.1:3000
#   export CUBE_TEMPLATE_ID=<template-id>
#   export CUBE_PROXY_NODE_IP=127.0.0.1             # loopback -> no gateway
#   export CUBE_VOLUME_DRIVER=cos                   # omit to use the default plugin
#   export CUBE_VOLUME_MOUNT_PATH=/workspace        # where to mount inside sandbox
#
# Usage: python3 integration_test_volume.py

import os
import sys
import uuid

# Import the SDK from source without installing.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox, Volume, VolumeMount  # noqa: E402

PASS = 0
FAIL = 0


def green(s):
    print("\033[32m  PASS: %s\033[0m" % s)


def red(s):
    print("\033[31m  FAIL: %s\033[0m" % s)


def yellow(s):
    print("\033[33m%s\033[0m" % s)


def assert_true(label, ok, detail=""):
    global PASS, FAIL
    if ok:
        PASS += 1
        green(label)ß
    else:
        FAIL += 1
        red("%s (%s)" % (label, detail))


def assert_eq(label, expected, actual):
    assert_true(label, expected == actual, "expected=%r actual=%r" % (expected, actual))


def main():
    api_url = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    driver = os.environ.get("CUBE_VOLUME_DRIVER") or None
    mount_path = os.environ.get("CUBE_VOLUME_MOUNT_PATH", "/workspace")

    if not template_id:
        sys.exit("CUBE_TEMPLATE_ID is required for the volume e2e test")

    # Unique name so re-runs don't collide. Matches ^[a-zA-Z0-9_-]+$.
    vol_name = "e2e-vol-%s" % uuid.uuid4().hex[:12]

    print("========================================")
    print(" Python SDK Volume E2E Tests")
    print(" CubeAPI: %s" % api_url)
    print(" Driver:  %s" % (driver or "<default>"))
    print(" Volume:  %s" % vol_name)
    print("========================================")
    print()

    volume_id = None
    sb_a = None
    sb_b = None
    try:
        # 1. Create volume. With a driver -> pins the plugin (e.g. cos);
        # without one -> e2b-compatible path (backend picks the first
        # configured plugin). Both go through the single Volume.create().
        print("[Test 1] Volume.create")
        if driver:
            vol = Volume.create(vol_name, driver=driver)
        else:
            vol = Volume.create(vol_name)
        volume_id = vol.volume_id
        assert_true("create returns a volume_id", bool(vol.volume_id), "empty volume_id")
        assert_eq("create echoes name", vol_name, vol.name)
        print()

        # 2. List volumes
        print("[Test 2] Volume.list")
        vols = Volume.list()
        ids = [v.volume_id for v in vols]
        assert_true("list includes new volume", volume_id in ids, "got %s" % ids)
        print()

        # 3. Get single volume
        print("[Test 3] Volume.get")
        got = Volume.get(volume_id)
        assert_eq("get returns same volume_id", volume_id, got.volume_id)
        print()

        # 4. Create sandbox A with the volume mounted, then write a probe file.
        print("[Test 4] Sandbox A: create(volume_mounts=...) + write")
        target = "%s/e2e_probe.txt" % mount_path.rstrip("/")
        payload = "cube-volume-e2e-%s" % uuid.uuid4().hex[:8]
        sb_a = Sandbox.create(
            template=template_id,
            volume_mounts=[VolumeMount(name=volume_id, path=mount_path)],
        )
        assert_true("sandbox A created", bool(sb_a.sandbox_id), "no sandbox id")
        print("  sandbox_a=%s" % sb_a.sandbox_id)
        sb_a.files.write(target, payload)
        # Same-sandbox read first: basic data-plane smoke test.
        same = sb_a.files.read(target)
        assert_eq("A: file round-trips within the same sandbox", payload, same)
        print()

        # 5. Kill A, then mount the SAME volume into a fresh sandbox B and read
        #    the file back. This is the real cross-sandbox persistence check:
        #    the bytes must have landed in the backing store (e.g. cos), not in
        #    sandbox A's local overlay.
        print("[Test 5] Sandbox B: remount same volume + read (cross-sandbox persistence)")
        sb_a.kill()
        sb_a = None
        green("sandbox A killed (detach volume before remount)")
        sb_b = Sandbox.create(
            template=template_id,
            volume_mounts=[VolumeMount(name=volume_id, path=mount_path)],
        )
        assert_true("sandbox B created", bool(sb_b.sandbox_id), "no sandbox id")
        print("  sandbox_b=%s" % sb_b.sandbox_id)
        content = sb_b.files.read(target)
        assert_eq("B: file persisted across sandboxes via the volume", payload, content)
        print()

    finally:
        # Cleanup: kill any live sandbox first (so the volume can be detached),
        # then delete the volume. Deleting a volume does NOT auto-detach it.
        for label, s in (("A", sb_a), ("B", sb_b)):
            if s is not None:
                try:
                    s.kill()
                    green("cleanup: sandbox %s killed" % label)
                except Exception as exc:  # noqa: BLE001
                    red("cleanup: sandbox %s kill failed (%s)" % (label, exc))
        if volume_id is not None:
            try:
                Volume.delete(volume_id)
                green("cleanup: volume deleted")
            except Exception as exc:  # noqa: BLE001
                red("cleanup: volume delete failed (%s)" % exc)

    total = PASS + FAIL
    print()
    print("========================================")
    if FAIL == 0:
        green("ALL PASSED: %d/%d tests" % (PASS, total))
    else:
        red("FAILED: %d/%d tests" % (FAIL, total))
        yellow(" Passed: %d/%d" % (PASS, total))
    print("========================================")
    sys.exit(FAIL)


if __name__ == "__main__":
    main()
