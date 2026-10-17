# SPDX-License-Identifier: Apache-2.0
"""Root-alias rejection in a disposable container with a read-only root mount."""
import json
import os
import tempfile
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request

from support.connect import BASE, HTTP


def main():
    # This fixture deliberately addresses root. Require kernel-enforced read-only
    # root protection so a regression cannot remove any root directory entries.
    assert os.statvfs('/').f_flag & os.ST_RDONLY, 'requires docker --read-only'
    with tempfile.TemporaryDirectory() as directory:
        link = Path(directory) / 'alias'
        link.symlink_to('/', target_is_directory=True)
        req = Request(BASE + '/filesystem.Filesystem/Remove',
                      json.dumps({'path': str(link) + '/..'}).encode(),
                      {'Content-Type': 'application/json', 'Connect-Protocol-Version': '1'})
        try:
            with HTTP.open(req, timeout=5) as response:
                raise AssertionError(f'root alias unexpectedly succeeded: {response.status}')
        except HTTPError as error:
            body = json.loads(error.read())
            assert body['code'] == 'invalid_argument', body
        assert link.is_symlink()
        with HTTP.open(BASE + '/health', timeout=2) as response:
            assert response.status == 204
    print('symlink/dot-dot root object refused before mutation on read-only root')


if __name__ == '__main__':
    main()
