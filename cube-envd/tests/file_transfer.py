# SPDX-License-Identifier: Apache-2.0
"""Retained signed-file, compose and concurrent-transfer public contracts."""
import base64
import hashlib
import gzip
import http.client
import json
import os
import secrets
import shlex
import time
import unittest
from urllib.parse import urlencode, urlsplit
from support.daemon import DaemonTestCase


class FileTransfer(DaemonTestCase):
    def test_compose_large_sources_and_requested_owner(self):
        root = '/tmp/compose-large-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        self.command('mkdir ' + root + '; dd if=/dev/zero of=' + root + '/source bs=1M count=70 2>/dev/null; printf tail > ' + root + '/tail')
        code, _, body = self.request('POST', '/files/compose', json.dumps({'source_paths': [root + '/source', root + '/tail'], 'destination': root + '/output'}))
        self.assertEqual(code, 200, body)
        code, _, body = self.request('GET', '/files?' + urlencode({'path': root + '/output'}), headers={'Range': 'bytes=73400318-'})
        self.assertEqual((code, body), (206, b'\0\0tail'))
        self.command('test ! -e ' + root + '/source && test ! -e ' + root + '/tail')
        self.command('printf private > ' + root + '/private; chmod 600 ' + root + '/private')
        destination = root + '/owned/sub/output'
        code, _, body = self.request('POST', '/files/compose', json.dumps({'source_paths': [root + '/private'], 'destination': destination, 'username': 'nobody'}))
        self.assertEqual(code, 200, body)
        code, _, body = self.request('POST', '/filesystem.Filesystem/Stat', json.dumps({'path': destination}), {'Content-Type': 'application/json'})
        self.assertEqual(code, 200, body)
        self.assertEqual(json.loads(body)['entry']['owner'], 'nobody')
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': destination}))[2], b'private')
        self.command('test ! -e ' + root + '/private')

    def test_stalled_file_transfers_allow_more_than_eight_jobs(self):
        root = '/tmp/file-concurrency-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        self.command('mkdir ' + root + '; dd if=/dev/urandom of=' + root + '/large bs=1M count=8 2>/dev/null')
        digest = self.command('sha256sum ' + root + '/large').split()[0].decode()
        url = urlsplit(getattr(self, 'endpoint', os.environ['ENVD_TEST_URL']))
        headers = getattr(self, 'proxy_headers', {})
        peers = []
        try:
            for _ in range(8):
                connection = http.client.HTTPConnection(url.hostname,url.port,timeout=10); peers.append(connection)
                connection.request('GET','/files?' + urlencode({'path':root+'/large'}),headers={**headers,'Accept-Encoding':'identity'})
                response = connection.getresponse(); self.assertEqual(response.status,200)
                # Keep response unread and alive as well as the connection.
                connection.stalled_response = response
            code, _, body = self.request('GET','/files?' + urlencode({'path':root+'/large'}),headers={'Accept-Encoding':'identity'})
            self.assertEqual(code,200); self.assertEqual(hashlib.sha256(body).hexdigest(),digest)
            self.assertEqual(self.request('GET','/health')[0],204)
        finally:
            for connection in peers: connection.close()
        peers = []
        try:
            for index in range(8):
                connection = http.client.HTTPConnection(url.hostname,url.port,timeout=10); peers.append(connection)
                connection.putrequest('POST','/files?' + urlencode({'path':root+'/'+str(index)}), skip_host=any(key.lower() == 'host' for key in getattr(self, 'proxy_headers', {})))
                for key,value in {**headers,'Content-Type':'application/octet-stream','Transfer-Encoding':'chunked'}.items():connection.putheader(key,value)
                connection.endheaders();connection.send(b'3\r\nabc\r\n')
            barrier = "import pathlib,time; root=pathlib.Path(" + repr(root) + "); deadline=time.monotonic()+5\nwhile not all((root/str(n)).exists() and (root/str(n)).stat().st_size==3 for n in range(8)):\n assert time.monotonic()<deadline, 'upload prefixes not visible before request EOF'\n time.sleep(.01)"
            self.command('python3 -c ' + shlex.quote(barrier))
            code, _, body = self.request('POST','/files?' + urlencode({'path':root+'/ninth'}),b'x',{'Content-Type':'application/octet-stream'})
            self.assertEqual(code,200,body)
            self.assertEqual(self.request('GET','/files?' + urlencode({'path':root+'/ninth'}))[2],b'x')
            self.assertEqual(self.request('GET','/health')[0],204)
        finally:
            for connection in peers:connection.close()
        for index in range(8):self.assertEqual(self.request('GET','/files?' + urlencode({'path':root+'/'+str(index)}))[2],b'abc')

    def test_compose_source_symlink_to_destination_is_removed(self):
        root = '/tmp/compose-alias-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        self.command('mkdir ' + root + '; printf original > ' + root + '/destination; ln -s destination ' + root + '/source')
        request = {'destination': root + '/destination', 'source_paths': [root + '/source']}
        code, _, body = self.request('POST', '/files/compose', json.dumps(request))
        self.assertEqual(code, 200, body)
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': root + '/destination'}))[2], b'original')
        self.command('test ! -L ' + root + '/source')

    def test_compose_order_cleanup_and_atomic_failure(self):
        root = '/tmp/compose-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        paths = [root + '/one', root + '/two']
        for path, data in zip(paths, [b'first', b'second']):
            self.assertEqual(self.request('POST', '/files?' + urlencode({'path': path}), data, {'Content-Type': 'application/octet-stream'})[0], 200)
        destination = root + '/sub/result'
        request = {'destination': destination, 'source_paths': paths}
        code, _, body = self.request('POST', '/files/compose', json.dumps(request))
        self.assertEqual(code, 200, body)
        self.assertEqual(json.loads(body), {'name': 'result', 'path': destination, 'type': 'file'})
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': destination}))[2], b'firstsecond')
        for path in paths:
            self.assertEqual(self.request('GET', '/files?' + urlencode({'path': path}))[0], 404)
        request['source_paths'] = [root + '/missing']
        self.assertEqual(self.request('POST', '/files/compose', json.dumps(request))[0], 404)
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': destination}))[2], b'firstsecond')
        for request, message in [({}, 'source_paths must not be empty'),
                                 ({'source_paths': paths}, 'destination is required'),
                                 ({'source_paths': [destination], 'destination': destination}, 'cannot be the same as destination')]:
            code, _, body = self.request('POST', '/files/compose', json.dumps(request))
            self.assertEqual(code, 400)
            self.assertIn(message, json.loads(body)['message'])
        for method in ['GET', 'PUT', 'HEAD']:
            code, headers, body = self.request(method, '/files/compose')
            self.assertEqual((code, body), (405, b''))
            self.assertEqual(headers['Allow'], 'POST')

    def test_signed_files(self):
        token = secrets.token_hex(32)
        self.assertEqual(self.request('POST', '/init', json.dumps({'accessToken': token}))[0], 204)
        path = '/tmp/envd-signed-' + secrets.token_hex(8)
        def signed(operation, expiration=None, username=None):
            text = ':'.join([path, operation, username or '', token])
            query = {'path': path}
            if expiration is not None:
                text += ':' + str(expiration)
                query['signature_expiration'] = str(expiration)
            if username is not None:
                query['username'] = username
            query['signature'] = 'v1_' + base64.b64encode(hashlib.sha256(text.encode()).digest()).decode().rstrip('=')
            return '/files?' + urlencode(query)
        content = {'Content-Type': 'application/octet-stream'}
        write = signed('write')
        read = signed('read')
        self.assertEqual(self.request('POST', write, b'kept', content)[0], 200)
        code, headers, body = self.request('GET', read, headers={'Origin':'https://example.test'})
        self.assertEqual((code, body), (200, b'kept'))
        self.assertEqual({part.strip() for value in headers.get_all('Vary', []) for part in value.split(',')}, {'Accept-Encoding'})
        self.assertEqual(headers.get('Access-Control-Allow-Origin'), '*')
        code, headers, body = self.request('GET', read, headers={'Accept-Encoding': 'gzip'})
        self.assertEqual((code, headers['Content-Encoding'], gzip.decompress(body)), (200, 'gzip', b'kept'))
        self.assertEqual(self.request('GET', read, headers={'Range': 'bytes=1-2'})[::2], (206, b'ep'))
        self.assertEqual(self.request('GET', read, headers={'If-None-Match': '*'})[::2], (304, b''))
        # HEAD and compose keep their ordinary token middleware; a file signature
        # only authorizes the supported GET/POST file-content routes.
        self.assertEqual(self.request('HEAD', read)[0], 401)
        code, headers, body = self.request('HEAD', read, headers={'X-Access-Token': token})
        self.assertEqual((code, body), (405, b''))
        self.assertEqual(headers.get_all('Allow'), ['GET', 'POST'])
        self.assertEqual(self.request('POST', '/files/compose' + read[len('/files'):], b'{}')[0], 401)
        self.assertEqual(self.request('POST', '/files/compose', b'{}', {'X-Access-Token': token})[0], 400)
        for uri in [signed('read', int(time.time()) - 60), read.replace('signature=', 'signature=x'), read.replace('path=', 'path=other')]:
            self.assertEqual(self.request('GET', uri)[0], 401)
        self.assertEqual(self.request('GET', signed('read', int(time.time()) + 60, 'root'))[::2], (200, b'kept'))
        self.assertEqual(self.request('POST', read, b'wrong', content)[0], 401)
        self.assertEqual(self.request('GET', read, headers={'X-Access-Token':'wrong'})[0], 401)
        self.assertEqual(self.request('GET', read)[::2], (200, b'kept'))
        for raw in ['', 'bad', '9223372036854775808']:
            self.assertEqual(self.request('GET', '/files?' + urlencode({'path':path, 'signature_expiration':raw}), headers={'X-Access-Token': token})[0], 400)
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': path, 'signature':''}), headers={'X-Access-Token':token})[::2], (200, b'kept'))
        self.assertEqual(self.request('POST', '/filesystem.Filesystem/Remove', json.dumps({'path':path}), {'Content-Type':'application/json', 'X-Access-Token':token})[0], 200)

class ComposeUmask(DaemonTestCase):
    daemon_umask = 0o027

    def test_compose_inherits_daemon_umask(self):
        self.assertTrue(hasattr(self, 'daemon_pid'), 'requires an isolated daemon with umask 027')
        root = '/tmp/compose-umask-' + secrets.token_hex(8)
        self.addCleanup(lambda: self.command('rm -rf ' + root))
        self.command('mkdir ' + root + '; printf content > ' + root + '/source')
        destination = root + '/sub/deep/output'
        code, _, body = self.request('POST', '/files/compose', json.dumps({'source_paths': [root + '/source'], 'destination': destination}))
        self.assertEqual(code, 200, body)
        self.assertEqual(self.command('stat -c %a ' + destination), b'640\n')
        self.assertEqual(self.command('stat -c %a ' + root + '/sub ' + root + '/sub/deep'), b'750\n750\n')
        self.assertEqual(self.request('GET', '/files?' + urlencode({'path': destination}))[2], b'content')
        self.command('test ! -e ' + root + '/source')

if __name__ == '__main__':
    unittest.main()
