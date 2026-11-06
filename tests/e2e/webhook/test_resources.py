# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Portable configuration and ownership checks; no services are modified."""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk_compat"))
from framework.webhook_resources import (owned_path, run_root, checked_unit,
    container_info, process_binary, endpoint)


class ResourceChecks(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="webhook-portability-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "arbitrary operator layout"
        self.root.mkdir()
        self.env = patch.dict(os.environ, {
            "WEBHOOK_P6_RUN_ROOT": str(self.root),
            "WEBHOOK_P6_RESOURCE_LABEL": "webhook-p6-portability",
        }, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_custom_root_and_symlink_escape(self):
        self.assertEqual(run_root(), self.root)
        self.assertEqual(owned_path(self.root / "logs/events.jsonl"), self.root / "logs/events.jsonl")
        (self.root / "escape").symlink_to(self.root.parent, target_is_directory=True)
        for path in (self.root, self.root / "../foreign", self.root / "escape/foreign", Path("relative")):
            with self.subTest(path=path), self.assertRaises(ValueError):
                owned_path(path)
        for root in ("/", "/tmp", "relative", ""):
            with self.subTest(root=root), patch.dict(os.environ, {"WEBHOOK_P6_RUN_ROOT": root}), self.assertRaises(ValueError):
                run_root()

    def test_container_must_match_run_label(self):
        good = [{"Config": {"Labels": {"purpose": "webhook-p6-portability"}}}]
        with patch("subprocess.check_output", return_value=json.dumps(good)) as inspect:
            container_info("webhook-1903-custom-redis")
            with self.assertRaises(ValueError):
                container_info("production-redis")
            self.assertEqual(inspect.call_count, 1)
        good[0]["Config"]["Labels"]["purpose"] = "webhook-p6-another-run"
        with patch("subprocess.check_output", return_value=json.dumps(good)), self.assertRaises(ValueError):
            container_info("webhook-1903-custom-redis")

    def test_unit_and_process_are_owned(self):
        # systemd escapes spaces in executable paths.
        binary = self.root / "bin/master-custom"
        escaped = str(binary).replace(" ", r"\x20")
        with patch("subprocess.check_output", return_value="{ path=" + escaped + " ; argv[]=master ; }"):
            self.assertEqual(checked_unit("webhook-1903-custom-master.service"), "webhook-1903-custom-master.service")
        with patch.dict(os.environ, {"WEBHOOK_P6_MASTER_BINARY": str(binary)}):
            with patch("subprocess.check_output", return_value=str(binary)):
                self.assertEqual(process_binary(123, "MASTER_BINARY"), binary)
            with patch("subprocess.check_output", return_value="/usr/bin/cubemaster"), self.assertRaises(ValueError):
                process_binary(123, "MASTER_BINARY")
        with patch("subprocess.check_output") as show, self.assertRaises(ValueError):
            checked_unit("production.service")
        show.assert_not_called()
        with patch("subprocess.check_output", return_value="{ path=/usr/bin/master ; argv[]=master ; }"), self.assertRaises(ValueError):
            checked_unit("webhook-1903-custom-master.service")

    def test_boot_plan_uses_inventory_without_starting_services(self):
        import host_recovery
        manifest = {"original_cubelet_restored": True, "run_root": str(self.root),
            "resource_label": "webhook-p6-portability", "receiver_url": "http://127.0.0.1:28124/events",
            "redis_db": "6", "webhook_redis": "webhook-1903-custom-redis",
            "business_redis": "webhook-1903-custom-business", "master_unit": "webhook-1903-custom-master.service",
            "ops_unit": "webhook-1903-custom-ops.service", "tasks": [{"key": "saved-task"}], "boot_id": "old-boot"}
        directory = self.root / "reboot"
        directory.mkdir()
        (directory / "armed.json").write_text(json.dumps(manifest))
        env = {"WEBHOOK_P6_" + key: value for key, value in {
            "RECEIVER_URL": manifest["receiver_url"], "REDIS_CONTAINER": manifest["webhook_redis"],
            "BUSINESS_REDIS_CONTAINER": manifest["business_redis"], "MASTER_UNIT": manifest["master_unit"],
            "OPS_UNIT": manifest["ops_unit"]}.items()}
        def inspect(args, **kwargs):
            if args[0] == "docker":
                return json.dumps([{"Config": {"Labels": {"purpose": manifest["resource_label"]}}}])
            return "{ path=" + str(self.root / "bin/custom").replace(" ", r"\x20") + " ; argv[]=custom ; }"
        with patch.dict(os.environ, env), patch.object(sys, "argv", ["host_recovery.py", str(self.root), "--verify-plan"]), \
                patch("subprocess.check_output", side_effect=inspect), patch("subprocess.run") as mutate:
            host_recovery.main()
        mutate.assert_not_called()
        self.assertTrue((directory / "armed.json").exists())

    def test_endpoint_uses_operator_port(self):
        with patch.dict(os.environ, {"WEBHOOK_P6_MASTER_URL": "http://127.0.0.1:28123/"}):
            self.assertEqual(endpoint("MASTER_URL"), "http://127.0.0.1:28123")
        with self.assertRaises(ValueError):
            endpoint("SECOND_MASTER_URL")


if __name__ == "__main__":
    unittest.main()
