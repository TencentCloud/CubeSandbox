import asyncio
import errno
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

import launch


class InterpreterLaunchTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.env = patch.dict(os.environ, {
            "CODE_INTERPRETER_HOST": "127.0.0.1", "CODE_INTERPRETER_PORT": "0",
            "CODE_INTERPRETER_WORKDIR": str(root / "work"),
            "ENVD_LOG_FILE": str(root / "envd.log"),
            "JUPYTER_LOG_FILE": str(root / "jupyter.log"),
        })
        self.env.start()
        self.addCleanup(self.env.stop)

    def children(self):
        return [Mock(poll=Mock(return_value=None)), Mock(poll=Mock(return_value=None))]

    def test_creates_custom_log_parent_directories(self):
        envd = Path(self.temp.name) / 'envd' / 'service.log'
        jupyter = Path(self.temp.name) / 'jupyter' / 'service.log'
        with patch.dict(os.environ, {'ENVD_LOG_FILE': str(envd), 'JUPYTER_LOG_FILE': str(jupyter)}), \
             patch.object(launch.subprocess, 'Popen', side_effect=self.children()), \
             patch.object(launch.uvicorn, 'Server'):
            launch.main()
        self.assertTrue(envd.is_file())
        self.assertTrue(jupyter.is_file())

    def assert_listening(self, listener):
        with socket.create_connection(listener.getsockname(), timeout=1):
            accepted, _ = listener.accept()
            accepted.close()

    def test_envd_uses_non_firecracker_mode_with_custom_port(self):
        children = self.children()
        with patch.dict(os.environ, {'ENVD_PORT': '50123'}), \
             patch.object(launch.subprocess, 'Popen', side_effect=children) as start, \
             patch.object(launch.uvicorn, 'Server'):
            launch.main()
        self.assertEqual(start.call_args_list[0].args[0],
                         ['/usr/bin/envd', '-isnotfc', '-port', '50123'])

    def test_listener_reserved_before_dependencies_and_lifespan(self):
        children = self.children()
        listener = None

        def construct_server(config):
            server = Mock()
            def run(*, sockets):
                nonlocal listener
                listener = sockets[0]
                self.assert_listening(listener)
                self.assertFalse(listener.get_inheritable())
                with socket.socket() as contender:
                    with self.assertRaises(OSError) as caught:
                        contender.bind(listener.getsockname())
                    self.assertEqual(caught.exception.errno, errno.EADDRINUSE)
            server.run.side_effect = run
            return server

        real_socket = socket.socket
        sockets = []
        def new_socket(*args, **kwargs):
            result = real_socket(*args, **kwargs);sockets.append(result);return result
        def start_child(*args, **kwargs):
            self.assertTrue(sockets)
            self.assert_listening(sockets[0])
            self.assertTrue(kwargs['close_fds'])
            return children.pop(0)
        with patch.object(launch.socket, 'socket', side_effect=new_socket), \
             patch.object(launch.subprocess, 'Popen', side_effect=start_child), \
             patch.object(launch.uvicorn, 'Server', side_effect=construct_server):
            launch.main()
        self.assertEqual(listener.fileno(), -1)

    def test_server_failure_reaps_both_children(self):
        children = self.children()
        with patch.object(launch.subprocess, 'Popen', side_effect=children), \
             patch.object(launch.uvicorn, 'Server') as server:
            server.return_value.run.side_effect = RuntimeError('startup failed')
            with self.assertRaisesRegex(RuntimeError, 'startup failed'):
                launch.main()
        for child in children:
            child.terminate.assert_called_once()
            child.wait.assert_called_once_with(timeout=5)

    def test_partial_child_launch_failure_reaps_started_child(self):
        child = self.children()[0]
        with patch.object(launch.subprocess, 'Popen', side_effect=[child, OSError('jupyter unavailable')]), \
             patch.object(launch.uvicorn, 'Server') as server:
            with self.assertRaisesRegex(OSError, 'jupyter unavailable'):
                launch.main()
        child.terminate.assert_called_once()
        server.assert_not_called()

    def test_failed_lifespan_returns_nonzero_and_reaps_children(self):
        children = self.children()
        with patch.object(launch.subprocess, 'Popen', side_effect=children), \
             patch.object(launch.uvicorn, 'Server') as server:
            server.return_value.started = False
            with self.assertRaises(SystemExit) as caught:
                launch.main()
        self.assertEqual(caught.exception.code, 3)
        for child in children:
            child.terminate.assert_called_once()

    def test_bind_conflict_does_not_start_children(self):
        with socket.socket() as occupied:
            occupied.bind(('127.0.0.1', 0))
            occupied.listen()
            with patch.dict(os.environ, {'CODE_INTERPRETER_PORT': str(occupied.getsockname()[1])}), \
                 patch.object(launch.subprocess, 'Popen') as start:
                with self.assertRaises(OSError) as caught:
                    launch.main()
        self.assertEqual(caught.exception.errno, errno.EADDRINUSE)
        start.assert_not_called()

    def test_cleanup_failure_still_reaps_other_child(self):
        children = self.children()
        children[1].terminate.side_effect = OSError('terminate failed')
        with self.assertRaisesRegex(OSError, 'terminate failed'):
            launch.stop_children(children)
        children[0].terminate.assert_called_once()

    def test_cleanup_failure_preserves_startup_failure(self):
        children = self.children()
        children[1].terminate.side_effect = OSError('terminate failed')
        with patch.object(launch.subprocess, 'Popen', side_effect=children), \
             patch.object(launch.uvicorn, 'Server') as server:
            server.return_value.run.side_effect = RuntimeError('startup failed')
            with self.assertRaisesRegex(RuntimeError, 'startup failed'):
                launch.main()
        children[0].terminate.assert_called_once()

    def test_stubborn_child_is_killed_and_reaped(self):
        child = self.children()[0]
        child.wait.side_effect = [subprocess.TimeoutExpired('child', 5), 0]
        launch.stop_child(child)
        child.kill.assert_called_once()
        self.assertEqual(child.wait.call_count, 2)

    def test_exited_child_needs_no_signal(self):
        child = Mock(poll=Mock(return_value=0))
        launch.stop_child(child)
        child.terminate.assert_not_called()

    def test_sigterm_exits_through_child_cleanup(self):
        children = self.children()
        previous = signal.getsignal(signal.SIGTERM)
        with patch.object(launch.subprocess, 'Popen', side_effect=children), \
             patch.object(launch.uvicorn, 'Server') as server:
            server.return_value.run.side_effect = lambda **kwargs: signal.raise_signal(signal.SIGTERM)
            with self.assertRaises(SystemExit) as caught:
                launch.main()
        self.assertEqual(caught.exception.code, 128 + signal.SIGTERM)
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)
        for child in children:
            child.terminate.assert_called_once()


class PreboundUvicornTest(unittest.TestCase):
    def test_http_is_ready_only_after_lifespan(self):
        entered = threading.Event()
        release = threading.Event()

        async def app(scope, receive, send):
            if scope['type'] == 'lifespan':
                await receive()
                entered.set()
                while not release.is_set():
                    await asyncio.sleep(0.01)
                await send({'type': 'lifespan.startup.complete'})
                await receive()
                await send({'type': 'lifespan.shutdown.complete'})
            else:
                await send({'type': 'http.response.start', 'status': 200, 'headers': []})
                await send({'type': 'http.response.body', 'body': b'ready'})

        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen(socket.SOMAXCONN)
            server = launch.uvicorn.Server(launch.uvicorn.Config(app, lifespan='on', log_level='error'))
            worker = threading.Thread(target=server.run, kwargs={'sockets': [listener]}, daemon=True)
            worker.start()
            try:
                self.assertTrue(entered.wait(3), 'Uvicorn did not enter startup')
                with socket.create_connection(listener.getsockname(), timeout=1) as client:
                    client.sendall(b'GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n')
                    client.settimeout(0.1)
                    with self.assertRaises(socket.timeout):
                        client.recv(4096)
                    release.set()
                    client.settimeout(3)
                    response = b''
                    while True:
                        data = client.recv(4096)
                        if not data:
                            break
                        response += data
                    self.assertIn(b'200 OK', response)
                    self.assertIn(b'ready', response)
            finally:
                release.set()
                server.should_exit = True
                worker.join(timeout=5)
                self.assertFalse(worker.is_alive(), 'Uvicorn shutdown did not complete')
