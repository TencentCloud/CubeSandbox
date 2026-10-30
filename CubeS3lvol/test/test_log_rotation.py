# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Offline checks for bounded log rotation; never touch deployment files."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('rotation', Path(__file__).parents[1] / 'scripts/rotate_logs.py')
rotation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rotation)


class RotationTest(unittest.TestCase):
    def test_release_includes_rotation_helper(self):
        release = (Path(__file__).parents[1] / 'make_release.sh').read_text()
        self.assertIn('rcow_purge.sh rotate_logs.py; do', release)

    def test_configuration_has_retention_and_size(self):
        text = rotation.configuration(Path('/tmp/a b.log'), Path('/tmp/archive'), 256, 8)
        self.assertIn('size 256', text)
        self.assertIn('rotate 8', text)
        self.assertIn('copytruncate', text)
        self.assertIn('su root root', text)
        self.assertIn('"/tmp/a b.log"', text)
        self.assertIn('olddir /tmp/archive\n', text)
        self.assertNotIn('postrotate', text)

    def test_control_character_rejected(self):
        with self.assertRaises(ValueError):
            rotation.configuration(Path('/tmp/a\n*.log'), Path('/tmp/archive'), 256, 8)

    def test_archive_token_and_glob_rejected(self):
        with self.assertRaises(ValueError):
            rotation.configuration(Path('/tmp/a.log'), Path('/tmp/a b'), 256, 8)
        with self.assertRaises(ValueError):
            rotation.configuration(Path('/tmp/*.log'), Path('/tmp/archive'), 256, 8)

    def test_missing_log_is_noop(self):
        with tempfile.TemporaryDirectory() as root:
            self.assertEqual(rotation.main(['--log', root + '/missing']), 0)

    def test_symlink_file_rejected(self):
        with tempfile.TemporaryDirectory() as root:
            p = Path(root);(p/'real').write_bytes(b'keep');(p/'link').symlink_to(p/'real')
            with self.assertRaises(RuntimeError):
                rotation.main(['--log', str(p/'link')])
            self.assertEqual((p/'real').read_bytes(), b'keep')

    def test_large_legacy_file_left_untouched(self):
        with tempfile.TemporaryDirectory() as root:
            p = Path(root)/'old';p.write_bytes(b'keep')
            with self.assertRaises(RuntimeError):
                rotation.main(['--log', str(p), '--bootstrap-limit', '3'])
            self.assertEqual(p.read_bytes(), b'keep')

    def test_space_guard_and_actual_invocation(self):
        with tempfile.TemporaryDirectory() as root:
            p = Path(root);log = p/'active';log.write_bytes(b'keep')
            args=['--log',str(log),'--archive-dir',str(p/'archive'),'--state-dir',str(p/'state'),'--size','1','--reserve-bytes','1']
            with patch.object(rotation.shutil,'disk_usage',return_value=type('Usage',(),{'free':4})()):
                with self.assertRaises(RuntimeError):rotation.main(args)
            with patch.object(rotation.shutil,'which',return_value='/usr/sbin/logrotate'), patch.object(rotation.subprocess,'run') as run:
                self.assertEqual(rotation.main(args+['--dry-run']),0)
                run.assert_not_called()
            self.assertEqual(log.read_bytes(),b'keep')

    def test_dry_run_does_not_create_state_or_archives(self):
        with tempfile.TemporaryDirectory() as root:
            p = Path(root)
            log = p / 'active'
            log.write_bytes(b'keep')
            self.assertEqual(rotation.main(['--log', str(log), '--state-dir', str(p / 'state'), '--archive-dir', str(p / 'archive'), '--dry-run']), 0)
            self.assertFalse((p / 'state').exists())
            self.assertFalse((p / 'archive').exists())

    def test_managed_log_can_recover_after_exceeding_bootstrap_limit(self):
        with tempfile.TemporaryDirectory() as root:
            p = Path(root)
            log = p / 'active'
            log.write_bytes(b'keep')
            state = p / 'state'
            state.mkdir()
            identity = rotation.hashlib.sha256(rotation.os.fsencode(log.resolve())).hexdigest()[:24]
            (state / (identity + '.state')).write_text('logrotate state -- version 2\n')
            with patch.object(rotation.shutil, 'which', return_value='/usr/sbin/logrotate'), patch.object(rotation.subprocess, 'run') as run:
                self.assertEqual(rotation.main(['--log', str(log), '--state-dir', str(state), '--bootstrap-limit', '3']), 0)
                run.assert_called_once()


if __name__ == '__main__':
    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(RotationTest))
    failed = len(result.errors) + len(result.failures)
    passed = result.testsRun - failed - len(result.skipped)
    print(f'result: {passed} passed, {failed} failed')
    raise SystemExit(0 if result.wasSuccessful() else 1)
