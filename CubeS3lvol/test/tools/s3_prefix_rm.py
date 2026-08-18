#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Delete every object under a prefix, or just list them.
#
#  === Why this exists at all ===
#
#  Nothing in s3lvol deletes an lvstore's objects: rcow_unload_lvstore
#  keeps them on purpose, since the next attach needs them. So every test run
#  leaves a prefix behind, and a crashed run also leaves an owner marker that
#  makes the *next* create fail with -EBUSY until it is removed. That turns
#  cleaning the bucket into a routine step rather than an occasional chore.
#
#  === Why it signs requests by hand ===
#
#  No aws-cli and no boto3 on the test hosts, and installing them is not always an
#  option. The target's own S3 client cannot be used either: cleaning up after a
#  crashed process is exactly the case where no target is running. Everything here
#  is stdlib.
#
#  === Usage ===
#
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    test/tools/s3_prefix_rm.py -e cos.ap-nanjing.myqcloud.com \
#                               -b my-bucket -r ap-nanjing -p dpvs/
#    test/tools/s3_prefix_rm.py ... --list          # show, delete nothing
#    test/tools/s3_prefix_rm.py ... -p ''           # the whole bucket
#
#  An empty prefix is allowed and means the entire bucket, so point it only at a
#  bucket dedicated to testing.

import argparse
import datetime
import hashlib
import hmac
import http.client
import os
import sys
import time
import urllib.parse
import xml.etree.ElementTree as ET

S3_NS = "{http://s3.amazonaws.com/doc/2006-03-01/}"
EMPTY_SHA = hashlib.sha256(b"").hexdigest()


class Client:
    def __init__(self, endpoint, bucket, region, path_style, ak, sk, no_tls=False):
        self.bucket = bucket
        self.region = region
        self.path_style = path_style
        self.no_tls = no_tls
        self.ak = ak
        self.sk = sk
        # Virtual-hosted style puts the bucket in the hostname and keeps the
        # canonical path clean; path style keeps one hostname and prefixes every
        # path with the bucket. Both have to be reflected in the signature, which
        # is why this is not just a URL difference.
        self.host = endpoint if path_style else "%s.%s" % (bucket, endpoint)

    def _base_path(self):
        return "/%s" % self.bucket if self.path_style else ""

    @staticmethod
    def _sign(key, msg):
        return hmac.new(key, msg.encode(), hashlib.sha256).digest()

    def request(self, method, path, query=""):
        """SigV4-signed request. Body is always empty, hence the fixed hash."""
        now = datetime.datetime.now(datetime.timezone.utc)
        amzdate = now.strftime("%Y%m%dT%H%M%SZ")
        datestamp = now.strftime("%Y%m%d")

        canonical_headers = ("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n"
                             % (self.host, EMPTY_SHA, amzdate))
        signed_headers = "host;x-amz-content-sha256;x-amz-date"
        canonical_request = "\n".join([method, path, query, canonical_headers,
                                       signed_headers, EMPTY_SHA])

        scope = "%s/%s/s3/aws4_request" % (datestamp, self.region)
        to_sign = "\n".join([
            "AWS4-HMAC-SHA256", amzdate, scope,
            hashlib.sha256(canonical_request.encode()).hexdigest()])

        k = self._sign(("AWS4" + self.sk).encode(), datestamp)
        k = self._sign(k, self.region)
        k = self._sign(k, "s3")
        k = self._sign(k, "aws4_request")
        signature = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()

        auth = ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, "
                "Signature=%s" % (self.ak, scope, signed_headers, signature))

        if self.no_tls:
            conn = http.client.HTTPConnection(self.host, timeout=60)
        else:
            conn = http.client.HTTPSConnection(self.host, timeout=60)
        try:
            conn.request(method, path + ("?" + query if query else ""),
                         headers={
                             "Host": self.host,
                             "x-amz-date": amzdate,
                             "x-amz-content-sha256": EMPTY_SHA,
                             "Authorization": auth,
                         })
            resp = conn.getresponse()
            return resp.status, resp.read()
        finally:
            conn.close()

    def list_keys(self, prefix):
        token = None
        while True:
            params = [("list-type", "2"), ("prefix", prefix),
                      ("max-keys", "1000")]
            if token:
                params.append(("continuation-token", token))
            # SigV4 requires the query string sorted by key and encoded exactly
            # as it goes on the wire.
            query = "&".join("%s=%s" % (k, urllib.parse.quote(v, safe="~"))
                             for k, v in sorted(params))
            status, body = self.request("GET", self._base_path() + "/", query)
            if status != 200:
                raise RuntimeError("list failed with HTTP %d\n%s"
                                   % (status, body[:800].decode(errors="replace")))
            root = ET.fromstring(body)
            for c in root.findall(S3_NS + "Contents"):
                yield c.findtext(S3_NS + "Key")
            if root.findtext(S3_NS + "IsTruncated") == "true":
                token = root.findtext(S3_NS + "NextContinuationToken")
            else:
                return

    def delete(self, key):
        # One DELETE per object rather than the batch POST: the batch needs a
        # signed body and per-key result parsing, and at test-bucket sizes the
        # round trips are not the slow part.
        path = self._base_path() + "/" + urllib.parse.quote(key)
        return self.request("DELETE", path)[0]


def main():
    p = argparse.ArgumentParser(
        description="delete every S3 object under a prefix")
    p.add_argument("-e", "--endpoint", required=True,
                   help="S3/COS endpoint host, e.g. cos.ap-nanjing.myqcloud.com")
    p.add_argument("-b", "--bucket", required=True)
    p.add_argument("-r", "--region", default="ap-nanjing")
    p.add_argument("-p", "--prefix", default="",
                   help="key prefix; empty means the whole bucket")
    p.add_argument("--path-style", action="store_true",
                   help="path-style addressing, for MinIO and the like")
    p.add_argument("--no-tls", action="store_true",
                   help="plain HTTP instead of HTTPS (MinIO and the like)")
    p.add_argument("--list", action="store_true",
                   help="list what would be deleted and stop")
    # Deleting in several passes because a listing is not a snapshot: objects
    # PUT shortly before it can be missing from the index and show up in the next
    # one. Seen for real -- a run that reported "362 deleted, 0 failed" left 177
    # objects that a listing seconds later did report, all of them written in the
    # final seconds of the test. One pass is therefore not "cleaned", it is
    # "cleaned as far as the index had caught up".
    p.add_argument("--passes", type=int, default=4,
                   help="how many list-then-delete rounds to run before giving "
                        "up (default: %(default)s); stops early once a listing "
                        "comes back empty")
    p.add_argument("--pass-delay", type=float, default=2.0,
                   help="seconds between rounds (default: %(default)s)")
    args = p.parse_args()

    # Credentials from the environment only, never from the command line: argv
    # is visible in ps output to every user on the host.
    try:
        ak = os.environ["AWS_ACCESS_KEY_ID"]
        sk = os.environ["AWS_SECRET_ACCESS_KEY"]
    except KeyError as e:
        print("%s is not set" % e.args[0], file=sys.stderr)
        return 2

    c = Client(args.endpoint, args.bucket, args.region, args.path_style, ak, sk,
               no_tls=args.no_tls)
    label = args.prefix or "(whole bucket)"

    if args.list:
        try:
            keys = list(c.list_keys(args.prefix))
        except (RuntimeError, ET.ParseError, OSError) as e:
            print(e, file=sys.stderr)
            return 1
        for k in keys:
            print(k)
        print("%d object(s) under %s" % (len(keys), label), file=sys.stderr)
        return 0

    deleted = 0
    failed = 0
    for attempt in range(1, args.passes + 1):
        try:
            keys = list(c.list_keys(args.prefix))
        except (RuntimeError, ET.ParseError, OSError) as e:
            print(e, file=sys.stderr)
            return 1

        if not keys:
            break

        if attempt > 1:
            print("%s: pass %d found %d more object(s)"
                  % (label, attempt, len(keys)), file=sys.stderr)

        for key in keys:
            try:
                status = c.delete(key)
            except OSError as e:
                status = -1
                print("  %s -> %s" % (key, e), file=sys.stderr)
            # 404 counts as success: the object is gone, which is the point.
            if status in (200, 204, 404):
                deleted += 1
            else:
                failed += 1
                print("  %s -> HTTP %s" % (key, status), file=sys.stderr)

        if attempt < args.passes:
            time.sleep(args.pass_delay)

    # Reported so a caller can tell "cleaned" from "cleaned as far as we could
    # see", but *not* treated as a failure: the delay cuts both ways -- a listing
    # taken right after a delete can still show the object. That was the case the
    # first time this was checked, and making it an error would turn every clean
    # run into a red one for no reason. Only an actual failed DELETE counts.
    left = -1
    try:
        left = sum(1 for _ in c.list_keys(args.prefix))
    except (RuntimeError, ET.ParseError, OSError):
        pass

    print("%s: %d deleted, %d failed%s"
          % (label, deleted, failed,
             "" if left == 0 else ", %s still listed (the index lags; usually "
             "already gone)" % ("unknown" if left < 0 else left)))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
