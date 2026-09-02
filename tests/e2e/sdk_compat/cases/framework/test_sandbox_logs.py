# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic tests for single-node cubecli log helpers."""

from __future__ import annotations

from unittest import mock

import pytest
from framework import sandbox_logs


pytestmark = pytest.mark.framework


def test_parse_ops_node_count_array():
    assert sandbox_logs.parse_ops_node_count([{"id": "a"}, {"id": "b"}]) == 2
    assert sandbox_logs.parse_ops_node_count([]) == 0


def test_parse_ops_node_count_wrapped():
    assert sandbox_logs.parse_ops_node_count({"data": [{"id": "a"}]}) == 1
    assert sandbox_logs.parse_ops_node_count({"nodes": []}) == 0
    assert sandbox_logs.parse_ops_node_count({"items": [{}, {}, {}]}) == 3


def test_parse_ops_node_count_rejects_unknown():
    assert sandbox_logs.parse_ops_node_count({"ok": True}) is None
    assert sandbox_logs.parse_ops_node_count("1") is None
    assert sandbox_logs.parse_ops_node_count(None) is None


def test_parse_master_nodes_scanned():
    text = "NODE_SCOPE       all\nNODES_SCANNED    1/1\nSANDBOX_COUNT    0\n"
    assert sandbox_logs.parse_master_nodes_scanned(text) == 1
    assert sandbox_logs.parse_master_nodes_scanned("NODES_SCANNED    2/3") == 3
    assert sandbox_logs.parse_master_nodes_scanned("no such line") is None
    assert sandbox_logs.parse_master_nodes_scanned("NODES_SCANNED    bogus") is None


def test_cubecli_logs_argv_sandbox_and_template():
    assert sandbox_logs.cubecli_logs_argv(
        "/opt/cubecli",
        "aabbccdd",
        address="/data/cubelet/cubelet.sock",
    ) == [
        "/opt/cubecli",
        "--address",
        "/data/cubelet/cubelet.sock",
        "logs",
        "--all",
        "aabbccdd",
    ]
    assert sandbox_logs.cubecli_logs_argv(
        "/opt/cubecli",
        "tpl-example",
        address="/data/cubelet/cubelet.sock",
        template=True,
        stream="stderr",
    ) == [
        "/opt/cubecli",
        "logs",
        "--tpl",
        "--stderr",
        "--all",
        "tpl-example",
    ]


def test_resolve_cubecli_prefers_env(tmp_path):
    cubecli = tmp_path / "cubecli"
    cubecli.write_text("#!/bin/sh\n", encoding="utf-8")
    cubecli.chmod(0o755)
    assert sandbox_logs.resolve_cubecli({"CUBECLI": str(cubecli)}) == str(cubecli)
    assert sandbox_logs.resolve_cubecli({"CUBECLI": str(tmp_path / "missing")}) is None


def test_skip_unless_single_node_skips_cluster():
    runner = mock.Mock(
        return_value=mock.Mock(returncode=0, stdout='[{"id":"a"},{"id":"b"}]')
    )
    with (
        mock.patch.object(sandbox_logs.shutil, "which", return_value="/bin/cubeopscli"),
        pytest.raises(pytest.skip.Exception, match="single-node only"),
    ):
        sandbox_logs.skip_unless_single_node_cubecli(runner=runner)


def test_skip_unless_single_node_skips_unknown_topology():
    runner = mock.Mock(return_value=mock.Mock(returncode=1, stdout=""))
    with (
        mock.patch.object(sandbox_logs.shutil, "which", return_value=None),
        pytest.raises(pytest.skip.Exception, match="could not determine"),
    ):
        sandbox_logs.skip_unless_single_node_cubecli(env={}, runner=runner)


def test_host_log_paths():
    stdout, stderr = sandbox_logs.host_log_paths("aabbccddeeff00112233445566778899")
    assert stdout == sandbox_logs.host_log_dir("aabbccddeeff00112233445566778899") / "stdout"
    assert stderr.name == "stderr"
    assert sandbox_logs.template_log_dir("tpl-example").name == "tpl-example_0"


def test_guest_envd_health_cmd():
    assert "127.0.0.1:49983/health" in sandbox_logs.GUEST_ENVD_HEALTH_CMD
    assert "cube-e2e-log" in sandbox_logs.guest_envd_probe_cmd("/cube-e2e-log")


def test_contains_envd_access_log():
    assert not sandbox_logs.contains_envd_access_log("")
    assert sandbox_logs.contains_envd_access_log('GET /health HTTP/1.1" 200')
    assert sandbox_logs.contains_envd_access_log("POST /process.Process/Start")
    assert sandbox_logs.contains_envd_access_log("Uvicorn running on http://0.0.0.0:49999")
    assert sandbox_logs.contains_envd_access_log("cube-e2e-log-deadbeef")
    assert sandbox_logs.count_envd_access('GET /health\nPOST /process.Process/Start\n') == 2


def test_count_envd_rpc():
    assert sandbox_logs.count_envd_rpc("") == 0
    text = (
        'INFO:     10.0.0.1:1 - "POST /process.Process/Start HTTP/1.1" 200 OK\n'
        'INFO:     10.0.0.1:2 - "POST /process.Process/Start HTTP/1.1" 200 OK\n'
    )
    assert sandbox_logs.count_envd_rpc(text) == 2


def test_skip_unless_single_node_skips_missing_cubecli(tmp_path):
    sock = tmp_path / "cubelet.sock"
    sock.write_text("", encoding="utf-8")
    runner = mock.Mock(return_value=mock.Mock(returncode=0, stdout='[{"id":"n1"}]'))
    env = {
        "CUBECLI": str(tmp_path / "missing"),
        "CUBELET_ADDRESS": str(sock),
        "CUBEOPSCLI": "/bin/cubeopscli",
    }
    with pytest.raises(pytest.skip.Exception, match="cubecli is not available"):
        sandbox_logs.skip_unless_single_node_cubecli(env=env, runner=runner)


def test_skip_unless_single_node_returns_paths(tmp_path):
    cubecli = tmp_path / "cubecli"
    cubecli.write_text("#!/bin/sh\n", encoding="utf-8")
    cubecli.chmod(0o755)
    sock = tmp_path / "cubelet.sock"
    sock.write_text("", encoding="utf-8")
    env = {
        "CUBECLI": str(cubecli),
        "CUBELET_ADDRESS": str(sock),
        "CUBEOPSCLI": "/bin/cubeopscli",
    }
    runner = mock.Mock(return_value=mock.Mock(returncode=0, stdout='[{"id":"n1"}]'))
    got_cli, got_addr = sandbox_logs.skip_unless_single_node_cubecli(
        env=env, runner=runner
    )
    assert got_cli == str(cubecli)
    assert got_addr == str(sock)
