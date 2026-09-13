# SPDX-License-Identifier: Apache-2.0
import hashlib
import http.client
import json
import os
import pathlib
import signal
import socket
import subprocess
import threading
import time
import unittest
import urllib.parse

from support.guest import (BINARY, CGROUP_MOUNT, State, MMDSHandler, CollectorHandler,
    Server, EchoServer, request, wait_for, cleanup_cgroup, free_port, isolate_network, GuestDaemon)

from support.daemon import DaemonTestCase


class StartupServices(DaemonTestCase):
    def setUp(self):
        if not BINARY:
            self.fail("ENVD_TEST_BINARY is required")
        State.metadata = {}
        State.mmds_requests = []
        State.collector_attempts = 0
        State.collected = []
        State.mmds_failures = 0
        State.mmds_hold = None
        State.collector_hold = None
        State.collector_release = None

    def test_mmds_projection_log_retry_refresh_and_shutdown(self):
        State.mmds_failures = 1
        collector_port = free_port()
        daemon_port = free_port()
        token = "guest-access-token"
        token_hash = hashlib.sha512(token.encode()).hexdigest()
        State.metadata = {
            "instanceID": "sandbox-initial",
            "envID": "template-initial",
            "address": f"http://127.0.0.1:{collector_port}/logs",
            "accessTokenHash": token_hash,
        }
        cgroup_root = CGROUP_MOUNT / f"envd-mmds-{os.getpid()}"
        cgroup_root.mkdir()
        (cgroup_root / "cgroup.subtree_control").write_text("+cpu +memory")
        environment = os.environ.copy()
        environment["NO_PROXY"] = "localhost,127.0.0.1,169.254.169.254,::1"
        environment["no_proxy"] = environment["NO_PROXY"]

        with Server(("169.254.169.254", 80), MMDSHandler), Server(
            ("127.0.0.1", collector_port), CollectorHandler
        ):
            daemon = subprocess.Popen(
                [
                    BINARY,
                    "-port",
                    str(daemon_port),
                    "-cgroup-root",
                    str(cgroup_root),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=environment,
                text=True,
            )
            try:
                wait_for(lambda: request(daemon_port, "GET", "/health")[0] == 204)

                def initial_environment():
                    status, _, body = request(daemon_port, "GET", "/envs")
                    values = json.loads(body)
                    return status == 200 and values.get("E2B_SANDBOX_ID") == "sandbox-initial"

                wait_for(initial_environment)
                for path, expected in [
                    ("/run/e2b/.E2B_SANDBOX_ID", b"sandbox-initial"),
                    ("/run/e2b/.E2B_TEMPLATE_ID", b"template-initial"),
                ]:
                    query = urllib.parse.urlencode({"path": path})
                    status, _, body = request(daemon_port, "GET", f"/files?{query}")
                    self.assertEqual((status, body), (200, expected))

                wait_for(lambda: State.collector_attempts >= 2 and State.collected)
                with State.lock:
                    headers, event = State.collected[0]
                normalized_headers = {key.lower(): value for key, value in headers.items()}
                self.assertEqual(normalized_headers["content-type"], "application/json")
                self.assertEqual(event["instanceID"], "sandbox-initial")
                self.assertEqual(event["envID"], "template-initial")
                serialized = json.dumps(event)
                self.assertNotIn(State.token, serialized)
                self.assertNotIn(token, serialized)
                self.assertNotIn(token_hash, serialized)

                with State.lock:
                    State.metadata = {
                        **State.metadata,
                        "instanceID": "sandbox-refreshed",
                        "envID": "template-refreshed",
                    }
                status, _, body = request(
                    daemon_port,
                    "POST",
                    "/init",
                    json.dumps({"accessToken": token}),
                    {"Content-Type": "application/json"},
                )
                self.assertEqual((status, body), (204, b""))

                def refreshed_environment():
                    status, _, body = request(
                        daemon_port,
                        "GET",
                        "/envs",
                        headers={"X-Access-Token": token},
                    )
                    values = json.loads(body)
                    return status == 200 and values.get("E2B_SANDBOX_ID") == "sandbox-refreshed"

                wait_for(refreshed_environment)
                self.endpoint = f'http://127.0.0.1:{daemon_port}'
                self.assertEqual(self.command('printf "%s/%s" "$E2B_SANDBOX_ID" "$E2B_TEMPLATE_ID"', token),
                                 b'sandbox-refreshed/template-refreshed')
                State.metadata = {**State.metadata, 'instanceID': 'must-not-project'}
                self.assertEqual(request(daemon_port, 'POST', '/init', json.dumps({'accessToken': 'wrong'}),
                                         {'Content-Type': 'application/json'})[0], 401)
                self.assertTrue(refreshed_environment())
                with State.lock:
                    requests = list(State.mmds_requests)
                self.assertTrue(
                    any(
                        method == "PUT"
                        and path == "/latest/api/token"
                        and {key.lower(): value for key, value in headers.items()}.get(
                            "x-metadata-token-ttl-seconds"
                        )
                        == "60"
                        for method, path, headers in requests
                    )
                )
                self.assertTrue(
                    any(
                        method == "GET"
                        and path == "/"
                        and {key.lower(): value for key, value in headers.items()}.get(
                            "x-metadata-token"
                        )
                        == State.token
                        and {key.lower(): value for key, value in headers.items()}.get("accept")
                        == "application/json"
                        for method, path, headers in requests
                    )
                )
            finally:
                if daemon.poll() is None:
                    daemon.send_signal(signal.SIGTERM)
                stdout, stderr = daemon.communicate(timeout=3)
                self.assertEqual(daemon.returncode, 0, stderr)
                self.assertNotIn(token, stdout + stderr)
                self.assertNotIn(token_hash, stdout + stderr)
                self.assertNotIn(State.token, stdout + stderr)

        cleanup_cgroup(cgroup_root)

    def test_tcp_forwarding_udp_absence_and_socat_cleanup(self):
        daemon_port = free_port()
        cgroup_root = CGROUP_MOUNT / f"envd-forward-{os.getpid()}"
        cgroup_root.mkdir()
        (cgroup_root / "cgroup.subtree_control").write_text("+cpu +memory")
        daemon = subprocess.Popen(
            [
                BINARY,
                "-isnotfc",
                "-port",
                str(daemon_port),
                "-cgroup-root",
                str(cgroup_root),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        echo = EchoServer()
        echo.start()
        try:
            wait_for(lambda: request(daemon_port, "GET", "/health")[0] == 204)

            def forwarded():
                with socket.create_connection(("169.254.0.21", echo.port), timeout=0.2) as client:
                    client.sendall(b"tcp-forward")
                    return client.recv(64) == b"tcp-forward"

            wait_for(forwarded)
            socat_pids = (cgroup_root / "socats" / "cgroup.procs").read_text().split()
            self.assertTrue(socat_pids)
            commands = {}
            for pid in socat_pids:
                try:
                    commands[pid] = (
                        pathlib.Path(f"/proc/{pid}/cmdline")
                        .read_bytes()
                        .replace(b"\0", b" ")
                    )
                except FileNotFoundError:
                    pass
            self.assertTrue(
                any(
                    str(echo.port).encode() in command and b"socat" in command
                    for command in commands.values()
                ),
                commands,
            )

            parent = next(
                int(pid)
                for pid, command in commands.items()
                if b"TCP4-LISTEN" in command
            )
            os.kill(parent, signal.SIGKILL)
            wait_for(forwarded, timeout=5)
            self.assertNotIn(
                str(parent),
                (cgroup_root / "socats" / "cgroup.procs").read_text().split(),
            )

            udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            udp.bind(("127.0.0.1", echo.port))
            udp.settimeout(0.3)
            udp.sendto(b"udp-not-forwarded", ("169.254.0.21", echo.port))
            with self.assertRaises(socket.timeout):
                udp.recvfrom(64)
            udp.close()

            echo.stop()
            wait_for(
                lambda: not (cgroup_root / "socats" / "cgroup.procs")
                .read_text()
                .strip(),
                timeout=5,
            )
            with self.assertRaises(OSError):
                socket.create_connection(("169.254.0.21", echo.port), timeout=0.2)
        finally:
            if echo.thread.is_alive():
                echo.stop()
            if daemon.poll() is None:
                daemon.send_signal(signal.SIGTERM)
            _, stderr = daemon.communicate(timeout=3)
            self.assertEqual(daemon.returncode, 0, stderr)

        cleanup_cgroup(cgroup_root)

    def test_non_firecracker_mode_does_not_read_mmds(self):
        daemon_port = free_port()
        cgroup_root = CGROUP_MOUNT / f"envd-non-fc-{os.getpid()}"
        cgroup_root.mkdir()
        (cgroup_root / "cgroup.subtree_control").write_text("+cpu +memory")
        with Server(("169.254.169.254", 80), MMDSHandler):
            daemon = subprocess.Popen(
                [
                    BINARY,
                    "-isnotfc",
                    "-port",
                    str(daemon_port),
                    "-cgroup-root",
                    str(cgroup_root),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            try:
                wait_for(lambda: request(daemon_port, "GET", "/health")[0] == 204)
                time.sleep(0.2)
                with State.lock:
                    self.assertEqual(State.mmds_requests, [])
            finally:
                if daemon.poll() is None:
                    daemon.send_signal(signal.SIGTERM)
                _, stderr = daemon.communicate(timeout=3)
                self.assertEqual(daemon.returncode, 0, stderr)

        cleanup_cgroup(cgroup_root)

    def test_forwarding_reaps_forked_handlers_and_recovers_missing_executable(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            environment = {**os.environ, 'PATH': directory}
            echo = EchoServer()
            echo.start()
            try:
                with GuestDaemon(environment=environment) as daemon:
                    wait_for(lambda: 'could not start localhost forwarding' in daemon.logs())
                    pathlib.Path(directory, 'socat').symlink_to('/usr/bin/socat')
                    def connect():
                        return socket.create_connection(('169.254.0.21', echo.port), timeout=.3)
                    client = wait_for(connect)
                    try:
                        client.sendall(b'alive')
                        self.assertEqual(client.recv(5), b'alive')
                        wait_for(lambda: len(daemon.helpers()) >= 2)
                        original = daemon.helpers()
                        parent = next(pid for pid in original if
                            int(pathlib.Path(f'/proc/{pid}/stat').read_text().split()[3]) == daemon.child.pid)
                        os.kill(parent, signal.SIGKILL)
                        wait_for(lambda: not original.intersection(daemon.helpers()))
                        self.assertEqual(client.recv(1), b'')
                        replacement = wait_for(connect)
                        try:
                            replacement.sendall(b'replaced')
                            self.assertEqual(replacement.recv(8), b'replaced')
                            wait_for(lambda: len(daemon.helpers()) >= 2)
                            daemon.stop(signal.SIGINT)
                            wait_for(lambda: not daemon.helpers())
                            self.assertEqual(replacement.recv(1), b'')
                        finally:
                            replacement.close()
                    finally:
                        client.close()
            finally:
                echo.stop()

    def test_ipv6_loopback_forwarding(self):
        try:
            echo = EchoServer(socket.AF_INET6)
        except OSError as error:
            import errno
            if error.errno == errno.EAFNOSUPPORT:
                self.skipTest('validation kernel does not support IPv6 sockets')
            raise
        echo.start()
        try:
            with GuestDaemon() as daemon:
                def forwarded():
                    with socket.create_connection(('169.254.0.21', echo.port), timeout=.3) as client:
                        client.sendall(b'ipv6')
                        return client.recv(4) == b'ipv6'
                wait_for(forwarded)
                self.assertTrue(daemon.helpers())
        finally:
            echo.stop()

    def test_metrics_environment_and_method_authorization(self):
        with GuestDaemon() as daemon:
            fields = {'ts', 'cpu_count', 'cpu_used_pct', 'mem_total', 'mem_used',
                      'mem_cache', 'mem_total_mib', 'mem_used_mib', 'disk_total', 'disk_used'}
            for _ in range(3):
                status, headers, body = request(daemon.port, 'GET', '/metrics')
                self.assertEqual(status, 200)
                self.assertEqual(headers['cache-control'], 'no-store')
                self.assertTrue(body.endswith(b'\n'))
                data = json.loads(body)
                self.assertEqual(set(data), fields)
                self.assertTrue(all(isinstance(v, (int, float)) and v >= 0 for v in data.values()))
                self.assertLessEqual(data['cpu_used_pct'], 100)
                self.assertEqual(data['mem_total_mib'], data['mem_total'] // 1024 // 1024)
                self.assertEqual(data['mem_used_mib'], data['mem_used'] // 1024 // 1024)
                self.assertLessEqual(data['disk_used'], data['disk_total'])
            token = 'auxiliary-test-secret'
            self.assertEqual(request(daemon.port, 'POST', '/init', json.dumps({
                'accessToken': token, 'envVars': {'GUEST_TEST': 'visible'}}),
                {'Content-Type': 'application/json'})[0], 204)
            for path in ('/metrics', '/envs'):
                self.assertEqual(request(daemon.port, 'GET', path)[0], 401)
                self.assertEqual(request(daemon.port, 'HEAD', path)[0], 401)
                headers = {'X-Access-Token': token, 'Origin': 'https://example.test'}
                status, response_headers, body = request(daemon.port, 'GET', path, headers=headers)
                self.assertEqual(status, 200)
                self.assertEqual(response_headers['access-control-allow-origin'], '*')
                for method in ('HEAD', 'POST'):
                    status, response_headers, body = request(daemon.port, method, path, headers=headers)
                    self.assertEqual(status, 405)
                    self.assertEqual(response_headers['allow'], 'GET')
                    if method == 'HEAD':
                        self.assertEqual(body, b'')
            self.endpoint = f'http://127.0.0.1:{daemon.port}'
            self.assertEqual(self.command('printf %s "$GUEST_TEST"', token), b'visible')
            self.assertNotIn(token, daemon.logs())

    def test_missing_controller_and_readonly_cgroup_fall_back_for_commands_and_forwarding(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            root = CGROUP_MOUNT / f'guest-no-controller-{os.getpid()}'
            root.mkdir()
            self.addCleanup(root.rmdir)

            def check(selected_root):
                marker = pathlib.Path(directory, 'ran')
                marker.unlink(missing_ok=True)
                port = free_port()
                with tempfile.TemporaryFile() as output:
                    daemon = subprocess.Popen(
                        [BINARY, '-isnotfc', '-port', str(port), '-cgroup-root',
                         str(selected_root), '-cmd', f'touch {marker}'],
                        stdout=output, stderr=output)
                    echo = EchoServer()
                    echo.start()
                    helpers = set()
                    try:
                        wait_for(lambda: request(port, 'GET', '/health')[0] == 204)
                        wait_for(marker.exists)
                        self.endpoint = f'http://127.0.0.1:{port}'
                        self.assertEqual(self.command('printf fallback'), b'fallback')
                        def forwarded():
                            with socket.create_connection(('169.254.0.21', echo.port), timeout=0.2) as client:
                                client.sendall(b'fallback-forward')
                                return client.recv(64) == b'fallback-forward'
                        wait_for(forwarded)
                        for children in pathlib.Path(f'/proc/{daemon.pid}/task').glob('*/children'):
                            for pid in children.read_text().split():
                                try:
                                    if b'socat' in pathlib.Path(f'/proc/{pid}/cmdline').read_bytes():
                                        helpers.add(int(pid))
                                except FileNotFoundError:
                                    pass
                        self.assertTrue(helpers)
                    finally:
                        echo.stop()
                        daemon.terminate()
                        try:
                            daemon.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            daemon.kill()
                            daemon.wait()
                            raise
                    logs = os.pread(output.fileno(), 1024 * 1024, 0).decode()
                    self.assertEqual(daemon.returncode, 0, logs)
                    self.assertEqual(logs.count('falling back to no-op cgroup manager'), 1, logs)
                    self.assertIn(str(selected_root), logs)
                    self.assertTrue(all(not pathlib.Path(f'/proc/{pid}').exists() for pid in helpers))
                self.assertEqual(list(root.glob('ptys')), [])
                self.assertEqual(list(root.glob('user')), [])

            check(root)
            mountpoint = pathlib.Path(directory, 'readonly')
            mountpoint.mkdir()
            subprocess.run(['mount', '--bind', str(root), str(mountpoint)], check=True)
            try:
                subprocess.run(['mount', '-o', 'remount,bind,ro', str(mountpoint)], check=True)
                check(mountpoint)
            finally:
                subprocess.run(['umount', str(mountpoint)], check=True)

    def test_shutdown_interrupts_pending_metadata_request(self):
        State.mmds_hold = threading.Event()
        with Server(('169.254.169.254', 80), MMDSHandler):
            with GuestDaemon(firecracker=True) as daemon:
                self.assertTrue(State.mmds_hold.wait(timeout=5))
                started = time.monotonic()
                daemon.stop()
                self.assertLess(time.monotonic() - started, 1.5)
                self.assertNotIn('critical task failed', daemon.logs())

    def test_shutdown_flushes_pending_logs_and_bounds_slow_collector(self):
        for release in (True, False):
            with self.subTest(release=release):
                State.collector_attempts = 1  # Skip the separate first-connection retry case.
                State.collected = []
                State.collector_hold = threading.Event()
                State.collector_release = threading.Event()
                port = free_port()
                State.metadata = {'instanceID': 'flush-sandbox', 'envID': 'flush-template',
                                  'address': f'http://127.0.0.1:{port}/logs'}
                with Server(('169.254.169.254', 80), MMDSHandler), Server(('127.0.0.1', port), CollectorHandler):
                    with GuestDaemon(firecracker=True) as daemon:
                        self.assertTrue(State.collector_hold.wait(timeout=5))
                        started = time.monotonic()
                        daemon.child.send_signal(signal.SIGTERM)
                        if release:
                            # The pending export now completes during the flush window.
                            State.collector_release.set()
                        daemon.stop()
                        self.assertLess(time.monotonic() - started, 1.5)
                        self.assertNotIn('critical task failed', daemon.logs())
                        if release:
                            wait_for(lambda: len(State.collected) >= 2)
                        State.collector_release.set()

    def test_metrics_sampling_failure_recovers_without_poisoning_health(self):
        import tempfile
        with tempfile.NamedTemporaryFile() as sample, GuestDaemon() as daemon:
            sample.write(b'cpu invalid\n')
            sample.flush()
            subprocess.run(['mount', '--bind', sample.name, '/proc/stat'], check=True)
            try:
                status, headers, body = request(daemon.port, 'GET', '/metrics')
                self.assertEqual((status, body), (500, b''))
                self.assertEqual(headers['cache-control'], 'no-store')
                self.assertEqual(request(daemon.port, 'GET', '/health')[0], 204)
            finally:
                subprocess.run(['umount', '/proc/stat'], check=True)
            self.assertEqual(request(daemon.port, 'GET', '/metrics')[0], 200)


if __name__ == "__main__":
    isolate_network()
    unittest.main()
