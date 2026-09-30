#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Exercise the image entrypoint across actual child-process boundaries."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

SUPERVISOR = Path(__file__).resolve().parents[1] / "cube-entrypoint.sh"
WORKER = '''#!/usr/bin/env python3
import json, os, pathlib, signal, sys, time
root = pathlib.Path(os.environ['TEST_ROOT'])
role = 'envd' if '-port' in sys.argv else 'user'
if role == 'user' and (root / 'spawn-child').exists() and os.fork() == 0:
    role = 'child'
def handle(sig, frame):
    with (root / (role + '.signals')).open('a') as out:
        out.write(str(sig) + '\\n')
    if (root / (role + '.stop-on-signal')).exists():
        sys.exit(91)
for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(sig, handle)
(root / (role + '.pid')).write_text(str(os.getpid()))
(root / (role + '.argv')).write_text(json.dumps(sys.argv))
while True:
    command = root / (role + '.exit')
    if command.exists():
        sys.exit(int(command.read_text()))
    time.sleep(.01)
'''


class SupervisorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.worker = self.root / 'worker'
        self.worker.write_text(WORKER)
        self.worker.chmod(0o755)
        self.user_worker = self.root / 'user-worker'
        self.user_worker.write_text(WORKER)
        self.user_worker.chmod(0o755)
        self.process = None

    def tearDown(self):
        if self.process:
            try:
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.process.communicate()
        for path in self.root.glob('*.pid'):
            try:
                os.kill(int(path.read_text()), signal.SIGKILL)
            except ProcessLookupError:
                pass
        self.temp.cleanup()

    def start(self, user=True):
        env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker), ENVD_LOG_FILE='-')
        self.process = subprocess.Popen(['bash', str(SUPERVISOR)] + ([str(self.user_worker)] if user else []),
                                        env=env, start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        for role in ('envd', 'user') if user else ('envd',):
            self.await_file(role + '.pid')

    def await_file(self, name):
        end = time.monotonic() + 3
        while time.monotonic() < end:
            path = self.root / name
            if path.exists() and path.read_text():
                return path.read_text()
            if self.process.poll() is not None:
                self.fail(self.process.communicate()[1].decode())
            time.sleep(.01)
        self.fail('timed out waiting for ' + name)

    def request_exit(self, role, status):
        # Publish the complete command atomically so workers cannot read a
        # newly created but empty (or partially written) exit-code file.
        pending = self.root / (role + '.exit.pending')
        pending.write_text(str(status))
        pending.replace(self.root / (role + '.exit'))

    def finish(self, status, cause):
        _, stderr = self.process.communicate(timeout=8)
        self.assertEqual(self.process.returncode, status, stderr.decode())
        self.assertIn('terminal cause=' + cause, stderr.decode())

    def test_user_status_survives_failed_daemon_shutdown(self):
        (self.root / 'envd.stop-on-signal').touch()
        self.start()
        self.request_exit('user', 23)
        self.finish(23, 'UserExit')


    def test_daemon_exit_fails_closed_even_when_clean(self):
        for status in (0, 42):
            with self.subTest(status=status):
                (self.root / 'user.stop-on-signal').touch()
                self.start()
                self.request_exit('envd', status)
                self.finish(status or 1, 'EnvdFailure')
                user_pid = int((self.root / 'user.pid').read_text())
                with self.assertRaises(ProcessLookupError):
                    os.kill(user_pid, 0)
                for path in self.root.glob('*.exit'):
                    path.unlink()
                for path in self.root.glob('*.pid'):
                    path.unlink()

    def test_external_signals_are_first_cause_and_forwarded_once(self):
        for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            with self.subTest(signal=sig):
                (self.root / 'envd.stop-on-signal').touch()
                self.start()
                self.process.send_signal(sig)
                self.assertEqual(self.await_file('user.signals'), str(int(sig)) + '\n')
                self.process.send_signal(sig)
                self.process.send_signal(signal.SIGTERM)
                self.request_exit('user', 0)
                self.finish(128 + sig, 'ExternalSignal')
                self.assertEqual((self.root / 'user.signals').read_text(), str(int(sig)) + '\n')
                for path in self.root.iterdir():
                    if path.name not in ('worker', 'user-worker'):
                        path.unlink()

    def test_external_signal_does_not_reach_user_descendant(self):
        (self.root / 'spawn-child').touch()
        (self.root / 'envd.stop-on-signal').touch()
        self.start()
        self.await_file('child.pid')
        self.process.send_signal(signal.SIGHUP)
        self.await_file('user.signals')
        time.sleep(.05)
        self.assertFalse((self.root / 'child.signals').exists())
        self.request_exit('child', 0)
        self.request_exit('user', 0)
        self.finish(129, 'ExternalSignal')

    def test_entrypoint_can_be_copied_alone_and_invoked_with_sh(self):
        # Existing image consumers COPY only this file, not a helper directory.
        copied = self.root / 'entry.sh'
        copied.write_bytes(SUPERVISOR.read_bytes())
        (self.root / 'envd.stop-on-signal').touch()
        env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker), ENVD_LOG_FILE='-')
        self.process = subprocess.Popen(['sh', str(copied), 'sh', '-c', 'exit 17'], env=env,
                                        start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.finish(17, 'UserExit')

    def test_immediate_user_exit_is_not_lost(self):
        (self.root / 'envd.stop-on-signal').touch()
        env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker), ENVD_LOG_FILE='-')
        self.process = subprocess.Popen(['bash', str(SUPERVISOR), 'sh', '-c', 'exit 17'], env=env,
                                        start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.finish(17, 'UserExit')

    def test_daemon_only_clean_exit_is_failure(self):
        self.start(user=False)
        self.request_exit('envd', 0)
        self.finish(1, 'EnvdFailure')

    def test_known_second_startup_contract_is_rejected(self):
        for command in (['/usr/bin/envd'], ['/bin/bash', '/usr/local/bin/cube-entrypoint.sh'],
                        ['sh', '-c', 'exec /usr/bin/envd'],
                        ['/usr/bin/tini', '--', '/usr/bin/envd']):
            with self.subTest(command=command):
                env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker), ENVD_LOG_FILE='-')
                self.process = subprocess.Popen(['bash', str(SUPERVISOR)] + command, env=env,
                                                start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                _, stderr = self.process.communicate(timeout=8)
                self.assertNotEqual(self.process.returncode, 0)
                self.assertIn('user command already starts envd', stderr.decode())
                self.assertFalse((self.root / 'envd.pid').exists())

    def test_unresponsive_user_shutdown_is_bounded(self):
        self.start()
        self.request_exit('envd', 44)
        self.finish(44, 'EnvdFailure')
        with self.assertRaises(ProcessLookupError):
            os.kill(int((self.root / 'user.pid').read_text()), 0)

    def test_extra_arguments_are_words_without_shell_evaluation(self):
        env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker),
                   ENVD_PORT='49984', ENVD_LOG_FILE='-',
                   ENVD_EXTRA_ARGS='--log-format json\n-isnotfc=false * $(touch marker)')
        self.process = subprocess.Popen(['bash', str(SUPERVISOR)], env=env,
                                        start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        argv = json.loads(self.await_file('envd.argv'))
        self.assertEqual(argv[1:], ['-port', '49984', '--log-format', 'json',
                                   '-isnotfc=false', '*', '$(touch', 'marker)', '-isnotfc'])
        self.request_exit('envd', 42)
        self.finish(42, 'EnvdFailure')

    def test_missing_executable_and_unwritable_log_fail_before_start(self):
        for overrides, expected in (({'ENVD_BIN': str(self.root / 'missing')}, 127),
                                    ({'ENVD_LOG_FILE': str(self.worker / 'log')}, 1)):
            with self.subTest(overrides=overrides):
                env = dict(os.environ, TEST_ROOT=str(self.root), ENVD_BIN=str(self.worker),
                           ENVD_LOG_FILE='-')
                env.update(overrides)
                self.process = subprocess.Popen(['bash', str(SUPERVISOR)], env=env,
                                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                                start_new_session=True)
                _, stderr = self.process.communicate(timeout=8)
                self.assertEqual(self.process.returncode, expected, stderr.decode())
                self.assertFalse((self.root / 'envd.pid').exists())


if __name__ == '__main__':
    unittest.main()
