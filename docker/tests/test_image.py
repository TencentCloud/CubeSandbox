# SPDX-License-Identifier: Apache-2.0
"""Real image checks. Set CUBE_ENVD_IMAGE, CUBE_ENVD_ARCH and CUBE_ENVD_COMMIT.

Requires a local Docker daemon with private cgroup v2 containers. Source is
mounted read-only; writable cgroup setup is confined to each disposable container.
"""
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import time
import unittest


def docker(*args, timeout=90):
    return subprocess.check_output(['docker', *args], text=True,
                                   stderr=subprocess.STDOUT, timeout=timeout).strip()


@unittest.skipUnless(os.environ.get('CUBE_ENVD_IMAGE'), 'CUBE_ENVD_IMAGE required')
class ImageTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.image = os.environ['CUBE_ENVD_IMAGE']
        cls.arch = os.environ['CUBE_ENVD_ARCH']
        cls.commit = os.environ['CUBE_ENVD_COMMIT']
        cls.provider = os.environ.get('CUBE_ENVD_PROVIDER', 'cube')
        cls.metadata = json.loads(docker('image', 'inspect', cls.image))[0]
        cls.entrypoint = cls.metadata['Config']['Entrypoint']

    def container(self, *command, env=(), prepare=True):
        args = ['create', '--network=none', '--cgroupns=private',
                '-e', 'NO_PROXY=localhost,127.0.0.1,::1',
                '-e', 'no_proxy=localhost,127.0.0.1,::1']
        for value in env:
            args += ['-e', value]
        if prepare:
            bootstrap = Path(__file__).with_name('prepare-cgroup.sh').resolve()
            args += ['--privileged', '-v', f'{bootstrap}:/prepare-cgroup.sh:ro',
                     '--entrypoint=/bin/bash', self.image,
                     '/prepare-cgroup.sh', *self.entrypoint, *command]
        else:
            args += [self.image, *command]
        cid = docker(*args)
        self.addCleanup(docker, 'rm', '-f', cid)
        config = json.loads(docker('inspect', cid))[0]['HostConfig']
        self.assertEqual(config['CgroupnsMode'], 'private')
        self.assertNotEqual(config['PidMode'], 'host')
        docker('start', cid)
        return cid

    def wait_for(self, cid, predicate, description, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            state = json.loads(docker('inspect', cid))[0]['State']
            if not state['Running']:
                self.fail(f'{description}: container exited: {docker("logs", cid)}')
            if predicate(state):
                return state
            time.sleep(.2)
        self.fail(f'{description}: timed out: {docker("logs", cid)}')

    def ready(self, cid):
        self.wait_for(cid, lambda state: state.get('Health', {}).get('Status') == 'healthy',
                      'healthcheck did not reach 204')

    def daemon_pid(self, cid):
        pids = docker('exec', cid, 'sh', '-c',
                      'for p in /proc/[0-9]*/exe; do '
                      '[ "$(readlink "$p")" = /usr/bin/envd ] && '
                      'echo "${p%/exe}"; done; true').splitlines()
        self.assertEqual(len(pids), 1, pids)
        return pids[0].split('/')[-1]

    def stopped(self, cid, expected):
        self.assertEqual(int(docker('wait', cid)), expected, docker('logs', cid))
        state = json.loads(docker('inspect', cid))[0]['State']
        self.assertFalse(state['Running'])
        self.assertEqual(state['Pid'], 0)

    def kill_daemon(self, cid, pid):
        # Without CMD, daemon death tears down the container and can kill this
        # docker-exec process too. Independently check the container status.
        result = subprocess.run(['docker', 'exec', cid, 'kill', '-KILL', pid],
                                capture_output=True, timeout=90)
        self.assertIn(result.returncode, (0, 137), result.stderr.decode())

    def test_installation_and_identity(self):
        self.assertEqual(self.metadata['Architecture'], self.arch)
        self.assertEqual(self.entrypoint,
                         ['/usr/bin/tini', '--', '/usr/local/bin/cube-entrypoint.sh'])
        self.assertFalse(self.metadata['Config'].get('Cmd'))
        labels = self.metadata['Config']['Labels']
        if self.provider == 'cube':
            self.assertEqual(labels['io.cubesandbox.envd.provider'], 'cube')
            self.assertEqual(labels['org.opencontainers.image.revision'], self.commit)
        else:
            self.assertEqual(labels['io.cubesandbox.envd.ref'], os.environ['ENVD_REF'])
        extra = '--log-format json' if self.provider == 'cube' else '-isnotfc'
        cid = self.container(env=('ENVD_LOG_FILE=-', 'ENVD_EXTRA_ARGS=' + extra))
        self.ready(cid)
        self.assertEqual(docker('exec', cid, 'stat', '-c', '%a:%u:%g', '/usr/bin/envd'), '755:0:0')
        self.assertEqual(docker('exec', cid, '/usr/bin/envd', '-commit'), self.commit)
        if self.provider == 'cube':
            self.assertEqual(docker('exec', cid, '/usr/bin/envd', 'cube-version'), '0.1.0')
            self.assertEqual(docker('exec', cid, '/usr/bin/envd', '-version'), '0.5.7')
        else:
            self.assertTrue(docker('exec', cid, '/usr/bin/envd', '-version'))
        self.assertEqual(docker('exec', cid, 'id', '-u', 'user'), '1000')
        if self.provider == 'cube':
            self.assertEqual(docker('exec', cid, 'sh', '-c', 'command -v socat'), '/usr/bin/socat')
        pid = self.daemon_pid(cid)
        caps = docker('exec', cid, 'cat', f'/proc/{pid}/status')
        for line in caps.splitlines():
            if line.startswith(('CapEff:', 'CapBnd:', 'CapInh:', 'CapAmb:')):
                self.assertFalse(int(line.split()[1], 16) & (1 << 25), line)
        logs = docker('logs', cid)
        startup = [json.loads(line)['fields'] for line in logs.splitlines()
                   if line.startswith('{') and 'starting cube-envd' in line]
        if self.provider == 'cube':
            self.assertEqual(len(startup), 1)
            self.assertEqual(startup[0]['provider'], 'cube')
            self.assertEqual(startup[0]['revision'], self.commit)
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / 'envd'
            docker('cp', f'{cid}:/usr/bin/envd', str(binary))
            data = binary.read_bytes()
        self.assertEqual(data[:6], b'\x7fELF\x02\x01')
        self.assertEqual(struct.unpack_from('<H', data, 18)[0], {'amd64': 62, 'arm64': 183}[self.arch])
        offset = struct.unpack_from('<Q', data, 32)[0]
        size, count = struct.unpack_from('<HH', data, 54)
        program_types = [struct.unpack_from('<I', data, offset + size * i)[0] for i in range(count)]
        self.assertNotIn(3, program_types, 'static executable must not need a PT_INTERP loader')
        self.kill_daemon(cid, pid)
        self.stopped(cid, 137)

    def test_arguments_environment_log_and_user_exit(self):
        extra = '--log-format json' if self.provider == 'cube' else '-isnotfc'
        cid = self.container('sh', '-c',
                             'printf "%s" "$TEST_VALUE" > /tmp/user-env; '
                             'while [ ! -e /tmp/exit-user ]; do sleep .1; done; exit 23',
                             env=('ENVD_PORT=49984', 'ENVD_EXTRA_ARGS=' + extra,
                                  'TEST_VALUE=retained value', 'ENVD_LOG_FILE=/var/log/custom/envd.log'))
        self.ready(cid)
        pid = self.daemon_pid(cid)
        argv = docker('exec', cid, 'cat', f'/proc/{pid}/cmdline').split('\0')
        expected = ['--log-format', 'json', '-isnotfc'] if self.provider == 'cube' else ['-isnotfc']
        self.assertEqual(argv[:-1], ['/usr/bin/envd', '-port', '49984', *expected])
        self.assertIn('TEST_VALUE=retained value', docker('exec', cid, 'cat', f'/proc/{pid}/environ'))
        self.assertEqual(docker('exec', cid, 'cat', '/tmp/user-env'), 'retained value')
        self.assertEqual(docker('exec', cid, 'test', '-f', '/var/log/custom/envd.log'), '')
        if self.provider == 'cube':
            self.assertIn('starting cube-envd', docker('exec', cid, 'cat', '/var/log/custom/envd.log'))
        docker('exec', cid, 'touch', '/tmp/exit-user')
        self.stopped(cid, 23)

    def test_failed_daemon_keeps_user_running(self):
        cid = self.container('sh', '-c',
                             'echo $$ > /tmp/user-pid; '
                             'while [ ! -e /tmp/exit-user ]; do sleep .1; done; exit 23',
                             env=('ENVD_LOG_FILE=-',))
        self.ready(cid)
        pid = self.daemon_pid(cid)
        self.kill_daemon(cid, pid)
        self.wait_for(cid, lambda state: state['Health']['Status'] == 'unhealthy',
                      'failed daemon must be unhealthy without stopping the application')
        docker('exec', cid, 'sh', '-c', 'kill -0 "$(cat /tmp/user-pid)"')
        docker('exec', cid, 'touch', '/tmp/exit-user')
        self.stopped(cid, 23)

    def test_external_signals_keep_waiting_for_application(self):
        cid = self.container('bash', '-c',
                             'trap "echo TERM >> /tmp/signals" TERM; '
                             'trap "echo INT >> /tmp/signals" INT; '
                             'trap "echo HUP >> /tmp/signals" HUP; '
                             'while [ ! -e /tmp/exit-user ]; do sleep .1; done; exit 23')
        self.ready(cid)
        pid = self.daemon_pid(cid)
        for sig in ('TERM', 'INT', 'HUP'):
            docker('kill', '--signal=' + sig, cid)
            deadline = time.monotonic() + 5
            while sig not in docker('exec', cid, 'sh', '-c', 'cat /tmp/signals 2>/dev/null || true').splitlines():
                if time.monotonic() > deadline:
                    self.fail(f'{sig} did not reach application')
                time.sleep(.05)
            self.assertEqual(self.daemon_pid(cid), pid)
        docker('exec', cid, 'touch', '/tmp/exit-user')
        self.stopped(cid, 23)

    def test_alive_but_unhealthy_daemon_is_visible(self):
        cid = self.container(env=('ENVD_LOG_FILE=-',))
        self.ready(cid)
        pid = self.daemon_pid(cid)
        docker('exec', cid, 'kill', '-STOP', pid)
        self.wait_for(cid, lambda state: state['Health']['Status'] == 'unhealthy',
                      'stopped daemon must be reported unhealthy')
        # Runtime/test cleanup owns forced termination, not the entrypoint.
        self.kill_daemon(cid, pid)
        self.stopped(cid, 137)

    def test_daemon_startup_failure_does_not_stop_user_command(self):
        cid = self.container('sh', '-c',
                             'while [ ! -e /tmp/exit-user ]; do sleep .1; done; exit 23',
                             env=('ENVD_EXTRA_ARGS=--port=70000', 'ENVD_LOG_FILE=-'))
        self.wait_for(cid, lambda state: state['Health']['Status'] == 'unhealthy',
                      'daemon startup failure must not stop the application')
        self.assertTrue(docker('logs', cid))
        docker('exec', cid, 'touch', '/tmp/exit-user')
        self.stopped(cid, 23)

    def test_readonly_cgroup_falls_back_and_preserves_supervision(self):
        cid = self.container('sh', '-c',
                             'trap "exit 23" TERM; while :; do sleep .1; done',
                             env=('ENVD_LOG_FILE=-',), prepare=False)
        self.ready(cid)
        self.daemon_pid(cid)
        self.assertIn('falling back to no-op cgroup manager', docker('logs', cid))
        docker('stop', '-t', '15', cid)
        self.stopped(cid, 23)

    def test_duplicate_command_and_missing_binary_fail(self):
        for command, env, message in ((('/usr/bin/envd',), (), 'already starts envd'),
                                      ((), ('ENVD_BIN=/missing-envd',), 'not executable')):
            with self.subTest(command=command, env=env):
                cid = self.container(*command, env=env, prepare=False)
                self.assertNotEqual(int(docker('wait', cid)), 0)
                self.assertIn(message, docker('logs', cid))


if __name__ == '__main__':
    unittest.main()
