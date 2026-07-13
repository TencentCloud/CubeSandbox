#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end integration test for the Python SDK filesystem API against a live
# envd instance. Uses the actual SDK Filesystem and Watcher classes with a
# minimal Sandbox shim that points directly at the sandbox IP.
#
# Requires Python 3.7+ and httpx. If the node's Python is too old, run from a
# machine with network access to the sandbox or use integration_test_filesystem.sh.
#
# Usage: python3 integration_test_sdk.py <SANDBOX_IP> [PORT]

import json
import os
import struct
import sys
import threading
import time

try:
    import httpx
except ImportError:
    sys.exit("httpx is required: pip install httpx")

# Add the SDK to the path so we can import it without installing
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox._filesystem import Filesystem


class _FakeSandbox:
    """Minimal shim so Filesystem can operate against a raw IP:port."""

    def __init__(self, host):
        self._host = host
        self._client = httpx.Client(timeout=httpx.Timeout(connect=10, read=30, write=30, pool=30))
        self._data = {}

    def get_host(self, port):
        return self._host

    def _build_data_client(self):
        return self._client

    def close(self):
        self._client.close()


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
        green(label)
    else:
        FAIL += 1
        red("%s (%s)" % (label, detail))


def assert_eq(label, expected, actual):
    assert_true(label, expected == actual, "expected=%r actual=%r" % (expected, actual))


def assert_contains(label, haystack, needle):
    assert_true(label, needle in str(haystack), "missing %r" % needle)


def main():
    if len(sys.argv) < 2:
        print("Usage: %s <SANDBOX_IP> [PORT]" % sys.argv[0], file=sys.stderr)
        sys.exit(1)

    host = sys.argv[1]
    port = sys.argv[2] if len(sys.argv) > 2 else "49983"
    target = "%s:%s" % (host, port)

    print("========================================")
    print(" Python SDK Filesystem E2E Tests")
    print(" Target: http://%s" % target)
    print("========================================")
    print()

    sb = _FakeSandbox(target)
    fs = Filesystem(sb)
    test_dir = "/tmp/py_sdk_e2e_%d" % os.getpid()

    try:
        # 1. MakeDir
        print("[Test 1] MakeDir")
        entry = fs.make_dir(test_dir)
        assert_true("make_dir returns dict", isinstance(entry, dict))
        assert_contains("make_dir type DIRECTORY", entry.get("type", ""), "DIRECTORY")
        print()

        # 2. MakeDir nested
        print("[Test 2] MakeDir nested")
        entry = fs.make_dir(test_dir + "/subdir")
        assert_contains("make_dir nested name", entry.get("name", ""), "subdir")
        print()

        # 3. Write file
        print("[Test 3] Write file")
        fs.write(test_dir + "/hello.txt", "Hello, Python SDK!")
        assert_true("write did not raise", True)
        print()

        # 4. Write second file
        print("[Test 4] Write nested file")
        fs.write(test_dir + "/subdir/nested.txt", "Nested content")
        assert_true("write nested did not raise", True)
        print()

        # 5. Stat file
        print("[Test 5] Stat file")
        entry = fs.stat(test_dir + "/hello.txt")
        assert_eq("stat name", "hello.txt", entry.get("name"))
        assert_contains("stat type FILE", entry.get("type", ""), "FILE_TYPE_FILE")
        assert_true("stat has size", "size" in entry, "missing size")
        assert_true("stat has permissions", "permissions" in entry, "missing permissions")
        print()

        # 6. Stat directory
        print("[Test 6] Stat directory")
        entry = fs.stat(test_dir)
        assert_contains("stat dir DIRECTORY", entry.get("type", ""), "DIRECTORY")
        print()

        # 7. List
        print("[Test 7] List")
        entries = fs.list(test_dir)
        names = [e.get("name") for e in entries]
        assert_true("list has hello.txt", "hello.txt" in names, "got %s" % names)
        assert_true("list has subdir", "subdir" in names, "got %s" % names)
        print()

        # 8. List nested
        print("[Test 8] List nested")
        entries = fs.list(test_dir + "/subdir")
        names = [e.get("name") for e in entries]
        assert_true("list nested has nested.txt", "nested.txt" in names, "got %s" % names)
        print()

        # 9. Read file
        print("[Test 9] Read file")
        content = fs.read(test_dir + "/hello.txt")
        assert_eq("read content", "Hello, Python SDK!", content)
        print()

        # 10. Exists
        print("[Test 10] Exists")
        assert_true("exists returns True", fs.exists(test_dir + "/hello.txt"))
        assert_true("exists returns False for missing", not fs.exists(test_dir + "/nope.txt"))
        print()

        # 11. Rename
        print("[Test 11] Rename")
        entry = fs.rename(test_dir + "/hello.txt", test_dir + "/renamed.txt")
        assert_contains("rename has renamed.txt", entry.get("name", ""), "renamed.txt")
        print()

        # 12. Verify old gone, new exists
        print("[Test 12] Exists after rename")
        assert_true("old path gone", not fs.exists(test_dir + "/hello.txt"))
        assert_true("new path exists", fs.exists(test_dir + "/renamed.txt"))
        print()

        # 13. Read renamed
        print("[Test 13] Read renamed")
        content = fs.read(test_dir + "/renamed.txt")
        assert_eq("renamed content preserved", "Hello, Python SDK!", content)
        print()

        # 14. WriteFiles
        print("[Test 14] WriteFiles")
        n = fs.write_files([
            (test_dir + "/batch_a.txt", "aaa"),
            (test_dir + "/batch_b.txt", "bbb"),
        ])
        assert_eq("write_files count", 2, n)
        assert_true("batch_a exists", fs.exists(test_dir + "/batch_a.txt"))
        assert_true("batch_b exists", fs.exists(test_dir + "/batch_b.txt"))
        print()

        # 15. Remove
        print("[Test 15] Remove")
        fs.remove(test_dir + "/renamed.txt")
        assert_true("remove did not raise", True)
        assert_true("removed file gone", not fs.exists(test_dir + "/renamed.txt"))
        print()

        # 16. Cleanup nested
        print("[Test 16] Cleanup nested")
        fs.remove(test_dir + "/subdir/nested.txt")
        fs.remove(test_dir + "/subdir")
        fs.remove(test_dir + "/batch_a.txt")
        fs.remove(test_dir + "/batch_b.txt")
        assert_true("cleanup ok", True)
        print()

        # 17. List empty
        print("[Test 17] List empty dir")
        entries = fs.list(test_dir)
        assert_eq("empty dir entries", 0, len(entries))
        print()

        # 18. Remove test dir + verify
        print("[Test 18] Remove test dir")
        fs.remove(test_dir)
        assert_true("test dir removed", not fs.exists(test_dir))
        print()

        # 19. WatchDir
        print("[Test 19] WatchDir")
        watch_dir = "/tmp/py_watch_%d" % os.getpid()
        fs.make_dir(watch_dir)

        watcher = fs.watch_dir(watch_dir)
        time.sleep(1)

        # trigger events
        fs.write(watch_dir + "/w.txt", "hello")
        time.sleep(0.5)
        fs.remove(watch_dir + "/w.txt")
        time.sleep(0.5)

        # collect events with timeout
        events = []

        def collect():
            try:
                for ev in watcher:
                    events.append(ev)
            except Exception:
                pass

        t = threading.Thread(target=collect)
        t.daemon = True
        t.start()
        t.join(timeout=5)
        watcher.close()

        event_types = [e.type for e in events]
        joined = ",".join(event_types)
        assert_true("WatchDir CREATE", "EVENT_TYPE_CREATE" in joined, "got: %s" % joined)
        assert_true("WatchDir WRITE", "EVENT_TYPE_WRITE" in joined, "got: %s" % joined)
        assert_true("WatchDir REMOVE", "EVENT_TYPE_REMOVE" in joined, "got: %s" % joined)

        # cleanup
        try:
            fs.remove(watch_dir)
        except Exception:
            pass
        print()

    finally:
        sb.close()

    # Summary
    total = PASS + FAIL
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
