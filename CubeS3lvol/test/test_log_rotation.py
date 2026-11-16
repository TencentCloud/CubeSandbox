#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Offline checks for storage log policy, installation and restart retention."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
POLICY_DIR = ROOT / 'deploy/one-click/scripts/logrotate'
INSTALLER = POLICY_DIR / 'install-logrotate.sh'


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


policy = load_module('s3lvol_policy', POLICY_DIR / 's3lvol_policy.py')
retention = load_module('prune_restart_logs', ROOT / 'CubeS3lvol/scripts/prune_restart_logs.py')


class RotationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.log = self.root / 's3lvol_tgt.log'
        self.log.write_text('original log\n', encoding='utf-8')
        self.env = dict(os.environ)
        for key in list(self.env):
            if key.startswith(('RCOW_LOG', 'ONE_CLICK_LOGROTATE_', 'CUBE_SANDBOX_LOGROTATE_')):
                del self.env[key]
        self.env.update(RCOW_LOG=str(self.log), ONE_CLICK_ENABLE_S3LVOL='1')

    def render(self, **settings):
        with patch.dict(os.environ, dict(self.env, **settings), clear=True):
            text = policy.render(POLICY_DIR / 's3lvol', POLICY_DIR / 's3lvol_policy.py')
        self.archive = next((self.root / 'rotated').iterdir()) if 'RCOW_LOG_ARCHIVE_DIR' not in settings else None
        return text

    def test_policy_uses_size_and_shared_framework(self):
        text = self.render()
        for value in ['size 256M', 'rotate 8', 'copytruncate', 'su root root', 'firstaction',
                      'createolddir 0700 root root']:
            self.assertIn(value, text)
        self.assertNotIn('hourly', text)
        # A prerotate refusal runs after archives were shifted and erodes them.
        self.assertNotRegex(text, r'(?m)^\s*prerotate\b')
        self.assertIn(str(self.log), text)

    def test_placeholder_names_in_paths_are_rendered_literally(self):
        self.log = self.root / 'at@ARCHIVE@.log'
        self.log.write_text('log\n', encoding='utf-8')
        text = self.render(RCOW_LOG=str(self.log))
        self.assertIn('"{}" {{'.format(self.log), text)
        self.assertIn('olddir {}\n'.format(self.archive), text)

    def test_check_space_recreates_a_removed_archive_directory(self):
        self.render()
        shutil.rmtree(self.root / 'rotated')
        with patch.object(policy.shutil, 'disk_usage', return_value=SimpleNamespace(free=2**62)):
            policy.check_space(self.log, self.archive)
        self.assertTrue(self.archive.is_dir())

    def test_log_dir_fallback(self):
        text = self.render(RCOW_LOG='', RCOW_LOG_DIR=str(self.root))
        self.assertIn(str(self.log), text)

    def test_paths_are_canonical_and_archives_are_per_source(self):
        alias = self.root / 'alias'
        alias.symlink_to(self.root, target_is_directory=True)
        self.assertEqual(self.render(), self.render(RCOW_LOG=str(alias / self.log.name)))
        first = self.archive
        self.render(RCOW_LOG=str(self.root / 'another.log'))
        self.assertEqual(len(list(first.parent.iterdir())), 2)

    def test_unsafe_paths_and_non_regular_logs_are_rejected(self):
        for value in ['relative.log', str(self.root / '*.log'), str(self.root / 'bad\nlog')]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.render(RCOW_LOG=value)
        for value in [str(self.root), str(self.root / 'bad dir'), str(self.root / 'bad\npath')]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.render(RCOW_LOG_ARCHIVE_DIR=value)
        link = self.root / 'link'
        link.symlink_to(self.log)
        with self.assertRaises(ValueError):
            self.render(RCOW_LOG=str(link))
        os.link(self.log, self.root / 'hardlink')
        with self.assertRaises(ValueError):
            self.render()

    def test_same_device_floor_covers_copy_and_delayed_compression(self):
        self.render()
        previous = self.archive / (self.log.name + '.1')
        previous.write_bytes(b'x' * 4096)
        needed = self.log.stat().st_size + previous.stat().st_size + 2**30
        with patch.object(policy.shutil, 'disk_usage', return_value=SimpleNamespace(free=needed - 1)):
            with self.assertRaisesRegex(ValueError, 'source left untouched'):
                policy.check_space(self.log, self.archive, reserve=1)
        with patch.object(policy.shutil, 'disk_usage', return_value=SimpleNamespace(free=needed)):
            policy.check_space(self.log, self.archive)

    def test_separate_device_floor_and_larger_override(self):
        self.render()
        source = self.log.stat()
        other_device = SimpleNamespace(st_dev=source.st_dev + 1)
        original_stat = Path.stat

        def archive_stat(path, *args, **kwargs):
            if path == self.archive:
                return other_device
            return original_stat(path, *args, **kwargs)

        with patch.object(Path, 'stat', archive_stat):
            with patch.object(policy.shutil, 'disk_usage', return_value=SimpleNamespace(free=source.st_size + 512 * 2**20)):
                policy.check_space(self.log, self.archive)
                with self.assertRaises(ValueError):
                    policy.check_space(self.log, self.archive, reserve=2**30)

    def test_guard_rejects_replaced_source(self):
        self.render()
        self.log.unlink()
        self.log.symlink_to(self.root / 'missing')
        with self.assertRaises(ValueError):
            policy.check_space(self.log, self.archive)

    def test_invalid_reserve_is_rejected(self):
        for value in ['0', '-1', 'bad']:
            with self.subTest(value=value), self.assertRaises((ValueError, policy.argparse.ArgumentTypeError)):
                self.render(RCOW_LOG_RESERVE_BYTES=value)

    def prepare_installer(self):
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.calls = self.root / 'systemctl.calls'
        self.shim = self.root / 'etc/logrotate.d/cubesandbox'
        self.units = self.root / 'units'
        self.storage = self.shim.parent / 's3lvol'
        for name, body in [('logrotate', 'exit 0\n'), ('systemctl', 'printf "%s\\n" "$*" >> "$TEST_CALLS"\n')]:
            executable = self.bin / name
            executable.write_text('#!/bin/sh\n' + body, encoding='utf-8')
            executable.chmod(0o755)
        self.env.update(
            PATH=str(self.bin) + os.pathsep + os.environ['PATH'],
            TEST_CALLS=str(self.calls), ONE_CLICK_LOGROTATE_ALLOW_NONROOT='1',
            ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL='1',
            CUBE_SANDBOX_LOGROTATE_POLICY_DST=str(self.shim),
            CUBE_SANDBOX_LOGROTATE_LEGACY_DST=str(self.root / 'legacy'),
            ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR=str(self.units))

    def install(self, **settings):
        return subprocess.run(['bash', str(INSTALLER)], env=dict(self.env, **settings),
                              text=True, capture_output=True, check=True)

    def install_without_logrotate(self, **settings):
        script = 'source "$1"; resolve_logrotate() { return 1; }; main'
        return subprocess.run(['bash', '-c', script, 'bash', str(INSTALLER)],
                              env=dict(self.env, **settings), text=True,
                              capture_output=True, check=True)

    def reject_storage_policies(self):
        (self.bin / 'logrotate').write_text(
            '#!/bin/sh\nfor last do :; done\nif grep -q "^[[:space:]]*copytruncate" "$last"; then\n'
            '  echo "error: rejected storage policy"\n  exit 1\nfi\n', encoding='utf-8')

    def assert_host_rotation_installed(self):
        self.assertEqual(self.shim.read_bytes(), (POLICY_DIR / 'cubesandbox').read_bytes())
        self.assertTrue((self.units / 'cube-sandbox-logrotate.timer').exists())
        self.assertIn('restart cube-sandbox-logrotate.timer', self.calls.read_text(encoding='utf-8'))

    def test_stale_override_recovers_both_policies_in_one_install(self):
        self.prepare_installer()
        result = self.install(ONE_CLICK_LOGROTATE_BIN=str(self.root / 'missing'))
        self.assertIn('trying system paths', result.stderr)
        self.assertTrue(self.shim.exists())
        self.assertTrue(self.storage.exists())
        service = (self.units / 'cube-sandbox-logrotate.service').read_text(encoding='utf-8')
        self.assertIn(str(self.bin / 'logrotate'), service)
        self.assertIn(str(self.shim.parent), service)
        self.assertNotIn('s3lvol-logrotate', self.calls.read_text(encoding='utf-8'))
        self.assertIn('restart cube-sandbox-logrotate.timer', self.calls.read_text(encoding='utf-8'))

    def test_storage_policy_removed_when_disabled(self):
        self.prepare_installer()
        self.install()
        self.install(ONE_CLICK_ENABLE_S3LVOL='0')
        self.assertFalse(self.storage.exists())
        self.assertTrue(self.shim.exists())

    def test_storage_policy_removed_when_disabled_without_logrotate(self):
        self.prepare_installer()
        self.install()
        result = self.install_without_logrotate(ONE_CLICK_ENABLE_S3LVOL='0')
        self.assertIn('logrotate missing', result.stderr)
        self.assertFalse(self.storage.exists())
        self.assertTrue(self.shim.exists())

    def test_real_installer_validates_custom_log_paths(self):
        binary = self.real_logrotate()
        self.prepare_installer()
        self.log = self.root / "storage's active log"
        self.log.write_text('custom path\n', encoding='utf-8')
        self.env['RCOW_LOG'] = str(self.log)
        result = self.install(ONE_CLICK_LOGROTATE_BIN=binary)
        self.assertTrue(self.storage.exists(), result.stderr)
        rotated = self.rotate(self.storage.read_text(encoding='utf-8'))
        self.assertEqual(rotated.returncode, 0, rotated.stderr)
        self.assertEqual(self.log.stat().st_size, 0)

    def test_real_installer_accepts_error_text_in_log_path(self):
        # --debug echoes the path ("rotating pattern: ..."); only a leading
        # "error:" is a logrotate diagnostic.
        binary = self.real_logrotate()
        self.prepare_installer()
        self.log = self.root / 'error:disk.log'
        self.log.write_text('log\n', encoding='utf-8')
        result = self.install(ONE_CLICK_LOGROTATE_BIN=binary, RCOW_LOG=str(self.log))
        self.assertNotIn('rejected', result.stderr)
        self.assertTrue(self.storage.exists(), result.stderr)

    def test_rejected_storage_policy_is_kept_while_host_rotation_is_refreshed(self):
        self.prepare_installer()
        self.install()
        original = self.storage.read_bytes()
        self.shim.write_text('previous host policy\n', encoding='utf-8')
        self.calls.unlink()
        self.reject_storage_policies()
        result = self.install(RCOW_LOG=str(self.root / 'replacement.log'))
        self.assertIn('rejected policy saved', result.stderr)
        self.assertIn('live storage policy left untouched', result.stderr)
        self.assertEqual(self.storage.read_bytes(), original)
        self.assertEqual(len(list(self.shim.parent.parent.glob('s3lvol.rejected.*'))), 1)
        self.assert_host_rotation_installed()

    def test_rejected_storage_policy_does_not_block_first_install(self):
        self.prepare_installer()
        self.reject_storage_policies()
        result = self.install()
        self.assertIn('storage log rotation is not installed', result.stderr)
        self.assertFalse(self.storage.exists())
        self.assert_host_rotation_installed()

    def test_unrenderable_policy_leaves_existing_storage_policy_untouched(self):
        self.prepare_installer()
        self.install()
        original = self.storage.read_bytes()
        self.calls.unlink()
        result = self.install(RCOW_LOG_ARCHIVE_DIR=str(self.root / 'bad dir'))
        self.assertIn('live storage policy left untouched', result.stderr)
        self.assertEqual(self.storage.read_bytes(), original)
        self.assert_host_rotation_installed()

    def test_unrenderable_policy_does_not_block_first_install(self):
        self.prepare_installer()
        result = self.install(RCOW_LOG='relative.log')
        self.assertIn('could not render storage log policy', result.stderr)
        self.assertFalse(self.storage.exists())
        self.assert_host_rotation_installed()

    def test_log_settings_reach_the_runtime_env(self):
        # The policy is rendered from the installer's environment and the
        # service reads .one-click.env: real template keys survive the upgrade
        # merge and the upsert keeps both sides on the same values.
        example = (ROOT / 'deploy/one-click/env.example').read_text(encoding='utf-8')
        installer = (ROOT / 'deploy/one-click/install.sh').read_text(encoding='utf-8')
        loop = installer[installer.index('for rcow_log_key in'):]
        loop = loop[:loop.index('done')]
        for key in ['RCOW_LOG_DIR', 'RCOW_LOG', 'RCOW_LOG_ARCHIVE_DIR',
                    'RCOW_LOG_RESERVE_BYTES', 'RCOW_LOG_RESTART_RETAIN']:
            with self.subTest(key=key):
                self.assertRegex(example, r'(?m)^{}='.format(key))
                self.assertRegex(loop, r'\b{}\b'.format(key))
        self.assertIn('upsert_env_kv "${RUNTIME_ENV_FILE}" "${rcow_log_key}"', loop)
        self.assertRegex(example, r'(?m)^RCOW_LOG_RESTART_RETAIN=8$')
        self.assertIn('RCOW_LOG_RESTART_RETAIN="${RCOW_LOG_RESTART_RETAIN-8}"', installer)

    def test_restart_retention_only_removes_matching_regular_files(self):
        names = [self.log.name + '.20000101-000000', self.log.name + '.20000101-000000~',
                 self.log.name + '.20000102-000000']
        for name in names:
            (self.root / name).write_text('archive', encoding='utf-8')
        link = self.root / (self.log.name + '.19990101-000000')
        link.symlink_to(self.log)
        hardlink = self.root / (self.log.name + '.19990102-000000')
        os.link(self.log, hardlink)
        unrelated = self.root / (self.log.name + '.1')
        unrelated.touch()
        retention.prune_restart_archives(self.log, 2)
        self.assertFalse((self.root / names[0]).exists())
        for path in [self.log, link, hardlink, unrelated] + [self.root / name for name in names[1:]]:
            self.assertTrue(path.exists())

    def test_restart_retention_rechecks_inode_before_removal(self):
        old = self.root / (self.log.name + '.20000101-000000')
        new = self.root / (self.log.name + '.20000102-000000')
        old.touch()
        new.touch()
        original = Path.lstat
        seen = 0

        def replaced(path):
            nonlocal seen
            info = original(path)
            if path == old:
                seen += 1
                if seen == 2:
                    return SimpleNamespace(st_mode=info.st_mode, st_nlink=1,
                                           st_dev=info.st_dev, st_ino=info.st_ino + 1)
            return info

        with patch.object(Path, 'lstat', replaced):
            retention.prune_restart_archives(self.log, 1)
        self.assertTrue(old.exists())

    def test_restart_retention_is_packaged_and_opt_in_at_restart(self):
        start = (ROOT / 'CubeS3lvol/scripts/rcow_start.sh').read_text(encoding='utf-8')
        package = (ROOT / 'CubeS3lvol/make_release.sh').read_text(encoding='utf-8')
        self.assertIn('if [ -n "${RCOW_LOG_RESTART_RETAIN:-}" ]; then', start)
        self.assertIn('prune_restart_logs.py', package)
        self.assertNotIn('prune_restart_logs', (POLICY_DIR / 's3lvol').read_text(encoding='utf-8'))

    def real_logrotate(self):
        binary = shutil.which('logrotate')
        if not binary or os.geteuid() != 0:
            self.skipTest('real logrotate checks require logrotate and root')
        return binary

    def rotate(self, text, force=True):
        config = self.root / 'policy'
        config.write_text(text, encoding='utf-8')
        command = [self.real_logrotate(), '--state', str(self.root / 'state')]
        if force:
            command.append('--force')
        return subprocess.run(command + [str(config)], text=True, capture_output=True)

    def test_real_rotation_preserves_open_descriptor_and_archive(self):
        self.real_logrotate()
        text = self.render()
        original = self.log.read_bytes()
        inode = self.log.stat().st_ino
        with self.log.open('ab') as writer:
            result = self.rotate(text)
            self.assertEqual(result.returncode, 0, result.stderr)
            writer.write(b'after rotation\n')
        self.assertEqual(self.log.stat().st_ino, inode)
        self.assertEqual(self.log.read_bytes(), b'after rotation\n')
        self.assertEqual((self.archive / (self.log.name + '.1')).read_bytes(), original)

    def test_real_space_refusal_does_not_truncate(self):
        self.real_logrotate()
        text = self.render(RCOW_LOG_RESERVE_BYTES=str(2**63))
        original = self.log.read_bytes()
        result = self.rotate(text)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('insufficient archive space', result.stderr)
        self.assertEqual(self.log.read_bytes(), original)
        self.assertFalse((self.archive / (self.log.name + '.1')).exists())

    def test_real_space_refusal_precedes_delayed_compression(self):
        self.real_logrotate()
        text = self.render(RCOW_LOG_RESERVE_BYTES=str(2**63))
        previous = self.archive / (self.log.name + '.1')
        previous.write_bytes(b'previous archive\n')
        original = self.log.read_bytes()
        result = self.rotate(text)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('insufficient archive space', result.stderr)
        self.assertEqual(self.log.read_bytes(), original)
        self.assertEqual(previous.read_bytes(), b'previous archive\n')
        self.assertEqual(list(self.archive.iterdir()), [previous])

    def test_real_repeated_refusals_keep_every_archive_generation(self):
        self.real_logrotate()
        text = self.render(RCOW_LOG_RESERVE_BYTES=str(2**63))
        (self.archive / (self.log.name + '.1')).write_bytes(b'generation 1\n')
        for index in range(2, 9):
            (self.archive / '{}.{}.gz'.format(self.log.name, index)).write_bytes(
                'generation {}\n'.format(index).encode())
        before = {path.name: path.read_bytes() for path in self.archive.iterdir()}
        original = self.log.read_bytes()
        for _ in range(3):
            result = self.rotate(text)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('insufficient archive space', result.stderr)
        self.assertEqual({path.name: path.read_bytes() for path in self.archive.iterdir()}, before)
        self.assertEqual(self.log.read_bytes(), original)

    def test_real_rotation_recreates_removed_archive_directory(self):
        self.real_logrotate()
        text = self.render()
        original = self.log.read_bytes()
        shutil.rmtree(self.root / 'rotated')
        result = self.rotate(text)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.archive / (self.log.name + '.1')).read_bytes(), original)
        self.assertEqual(self.log.stat().st_size, 0)

    def test_frequent_checks_preserve_hourly_policy(self):
        self.real_logrotate()
        hourly = self.root / 'shim.log'
        hourly.write_text('first\n', encoding='utf-8')
        text = self.render().replace('size 256M', 'size 1')
        text += '\n"{}" {{\n su root root\n hourly\n rotate 2\n create 0640 root root\n}}\n'.format(hourly)
        first = self.rotate(text)
        self.assertEqual(first.returncode, 0, first.stderr)
        hourly.write_text('second\n', encoding='utf-8')
        self.log.write_text('second storage log\n', encoding='utf-8')
        second = self.rotate(text, force=False)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(hourly.read_text(encoding='utf-8'), 'second\n')
        self.assertFalse(Path(str(hourly) + '.2').exists())
        self.assertEqual(self.log.stat().st_size, 0)


if __name__ == '__main__':
    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(RotationTest))
    failed = len(result.errors) + len(result.failures)
    passed = result.testsRun - failed - len(result.skipped)
    print('result: {} passed, {} failed'.format(passed, failed))
    if result.skipped:
        print('skipped: {}'.format(len(result.skipped)))
    raise SystemExit(0 if result.wasSuccessful() else 1)
