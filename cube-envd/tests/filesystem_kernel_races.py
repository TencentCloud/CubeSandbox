# SPDX-License-Identifier: Apache-2.0
"""Run the FUSE race scenarios in a disposable privileged test container."""
import os
from pathlib import Path
import subprocess
import sys
import unittest

from support.daemon import DaemonTestCase


class FilesystemKernelRaces(DaemonTestCase):
    def setUp(self):
        self.assertTrue(Path('/.dockerenv').is_file(), 'requires a disposable test container')
        self.assertTrue(os.environ.get('ENVD_TEST_BINARY'), 'set ENVD_TEST_BINARY to the new daemon')
        super().setUp()

    def run_scenario(self, script):
        subprocess.run(
            [sys.executable, '-B', str(Path(__file__).with_name(script))],
            env={**os.environ, 'ENVD_SMOKE_URL': os.environ['ENVD_TEST_URL']},
            check=True,
        )

    def test_creation(self):
        self.run_scenario('filesystem_creation_race.py')

    def test_list(self):
        self.run_scenario('filesystem_list_races.py')

    def test_mutation(self):
        self.run_scenario('filesystem_mutation_races.py')


if __name__ == '__main__':
    unittest.main()
