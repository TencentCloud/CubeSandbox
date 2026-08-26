#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Create (or confirm) an S3 bucket. Pure stdlib, same bargain as
#  s3_prefix_rm.py: no aws-cli, no boto3. Used by the one-click supervisor
#  before rcow_start.sh, because s3lvol will not create the bucket itself.
#
#  === Usage ===
#
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    s3_bucket.py ensure -e 127.0.0.1:9000 -b cube-s3lvol -r us-east-1 \
#                        --path-style --no-tls
#
#  Credentials from the environment only: argv is visible in ps.

import argparse
import datetime
import hashlib
import hmac
import http.client
import os
import sys
import urllib.parse
import xml.etree.ElementTree as ET

EMPTY_SHA = hashlib.sha256(b"").hexdigest()

CREATE_BUCKET_XML = (
    '<?xml version="1.0" encoding="UTF-8"?>'
    '<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    "<LocationConstraint>%s</LocationConstraint>"
    "</CreateBucketConfiguration>"
)


def _sign(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def host_for(endpoint, bucket, path_style):
    return endpoint if path_style else "%s.%s" % (bucket, endpoint)


def path_for(bucket, path_style, extra=""):
    # Virtual-hosted: canonical path is "/" (never empty -- SigV4 rejects "").
    # Path-style: "/<bucket>" plus any extra (unused today).
    if path_style:
        base = "/%s" % bucket
        return base + extra if extra else base
    return extra if extra else "/"


def payload_hash(body):
    return hashlib.sha256(body).hexdigest()


def sign_request(method, host, path, query, body, region, ak, sk, amzdate, datestamp):
    """Return (authorization, payload_sha). Deterministic given amzdate."""
    body = body if body is not None else b""
    payload_sha = payload_hash(body)
    canonical_headers = ("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n"
                         % (host, payload_sha, amzdate))
    signed_headers = "host;x-amz-content-sha256;x-amz-date"
    canonical_request = "\n".join([method, path, query, canonical_headers,
                                   signed_headers, payload_sha])
    scope = "%s/%s/s3/aws4_request" % (datestamp, region)
    to_sign = "\n".join([
        "AWS4-HMAC-SHA256", amzdate, scope,
        hashlib.sha256(canonical_request.encode()).hexdigest()])
    k = _sign(("AWS4" + sk).encode(), datestamp)
    k = _sign(k, region)
    k = _sign(k, "s3")
    k = _sign(k, "aws4_request")
    signature = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
    auth = ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, "
            "Signature=%s" % (ak, scope, signed_headers, signature))
    return auth, payload_sha


class Client:
    def __init__(self, endpoint, bucket, region, path_style, ak, sk, no_tls=False):
        self.bucket = bucket
        self.region = region
        self.path_style = path_style
        self.no_tls = no_tls
        self.ak = ak
        self.sk = sk
        self.host = host_for(endpoint, bucket, path_style)

    def request(self, method, path, query="", body=b""):
        now = datetime.datetime.now(datetime.timezone.utc)
        amzdate = now.strftime("%Y%m%dT%H%M%SZ")
        datestamp = now.strftime("%Y%m%d")
        body = body if body is not None else b""
        auth, payload_sha = sign_request(
            method, self.host, path, query, body, self.region,
            self.ak, self.sk, amzdate, datestamp)

        if self.no_tls:
            conn = http.client.HTTPConnection(self.host, timeout=60)
        else:
            conn = http.client.HTTPSConnection(self.host, timeout=60)
        try:
            headers = {
                "Host": self.host,
                "x-amz-date": amzdate,
                "x-amz-content-sha256": payload_sha,
                "Authorization": auth,
            }
            if body:
                headers["Content-Type"] = "application/xml"
                headers["Content-Length"] = str(len(body))
            conn.request(method, path + ("?" + query if query else ""),
                         body=body, headers=headers)
            resp = conn.getresponse()
            return resp.status, resp.read()
        finally:
            conn.close()

    def head_bucket(self):
        return self.request("HEAD", path_for(self.bucket, self.path_style))

    def put_bucket(self):
        body = b""
        if self.region and self.region not in ("us-east-1", "auto"):
            body = (CREATE_BUCKET_XML % self.region).encode()
        return self.request("PUT", path_for(self.bucket, self.path_style),
                            body=body)


def _already_exists(status, body):
    if status in (200, 204):
        return True
    if status != 409:
        return False
    text = body.decode(errors="replace") if body else ""
    return "BucketAlreadyOwnedByYou" in text or "BucketAlreadyExists" in text


def ensure(client):
    """HEAD then PUT. 200/403 on HEAD means the bucket is there (403 is a
    common 'exists but HeadBucket is denied' response). 404 triggers create.
    409 on create is treated as success."""
    status, body = client.head_bucket()
    if status in (200, 403):
        return 0, "exists (HEAD %d)" % status
    if status != 404:
        return 1, "HEAD returned HTTP %d\n%s" % (
            status, body[:800].decode(errors="replace"))

    status, body = client.put_bucket()
    if status in (200, 204) or _already_exists(status, body):
        return 0, "created (PUT %d)" % status
    return 1, "PUT returned HTTP %d\n%s" % (
        status, body[:800].decode(errors="replace"))


def self_test():
    passed = 0
    failed = 0

    def check(name, cond, detail=""):
        nonlocal passed, failed
        if cond:
            passed += 1
            print("  [PASS] %s" % name)
        else:
            failed += 1
            print("  [FAIL] %s%s" % (name, (" -- " + detail) if detail else ""))

    check("path-style host is the endpoint",
          host_for("127.0.0.1:9000", "cube-s3lvol", True) == "127.0.0.1:9000")
    check("virtual-hosted host prefixes the bucket",
          host_for("s3.amazonaws.com", "cube-s3lvol", False)
          == "cube-s3lvol.s3.amazonaws.com")
    check("path-style path is /bucket",
          path_for("cube-s3lvol", True) == "/cube-s3lvol")
    check("virtual-hosted path is / not empty",
          path_for("cube-s3lvol", False) == "/")

    ak = "AKIAIOSFODNN7EXAMPLE"
    sk = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
    amzdate = "20130524T000000Z"
    datestamp = "20130524"
    auth1, sha1 = sign_request(
        "HEAD", "127.0.0.1:9000", "/cube-s3lvol", "", b"",
        "us-east-1", ak, sk, amzdate, datestamp)
    auth2, sha2 = sign_request(
        "HEAD", "127.0.0.1:9000", "/cube-s3lvol", "", b"",
        "us-east-1", ak, sk, amzdate, datestamp)
    check("empty body hashes to EMPTY_SHA", sha1 == EMPTY_SHA)
    check("signature is deterministic", auth1 == auth2)
    check("authorization uses SigV4", auth1.startswith("AWS4-HMAC-SHA256 Credential="))

    body = (CREATE_BUCKET_XML % "ap-guangzhou").encode()
    _, sha_body = sign_request(
        "PUT", "cos.ap-guangzhou.myqcloud.com", "/cube-s3lvol", "", body,
        "ap-guangzhou", ak, sk, amzdate, datestamp)
    check("non-empty body changes the payload hash", sha_body != EMPTY_SHA)
    check("payload hash matches sha256(body)", sha_body == payload_hash(body))

    # LocationConstraint XML must remain well-formed -- a typo here would
    # create the bucket in the wrong region or fail the PUT.
    try:
        ET.fromstring(CREATE_BUCKET_XML % "ap-guangzhou")
        xml_ok = True
    except ET.ParseError:
        xml_ok = False
    check("CreateBucketConfiguration XML parses", xml_ok)

    print("result: %d passed, %d failed" % (passed, failed))
    return 0 if failed == 0 else 1


def main():
    p = argparse.ArgumentParser(description="ensure an S3 bucket exists")
    p.add_argument("command", nargs="?", default="ensure",
                   choices=("ensure",),
                   help="ensure: HEAD then create if missing (default)")
    p.add_argument("-e", "--endpoint", help="S3 endpoint host, no scheme")
    p.add_argument("-b", "--bucket", help="bucket name")
    p.add_argument("-r", "--region", default="us-east-1")
    p.add_argument("--path-style", action="store_true")
    p.add_argument("--no-tls", action="store_true")
    p.add_argument("--self-test", action="store_true",
                   help="run offline construction/signature checks and exit")
    args = p.parse_args()

    if args.self_test:
        return self_test()

    if not args.endpoint or not args.bucket:
        print("ensure needs -e/--endpoint and -b/--bucket", file=sys.stderr)
        return 2

    try:
        ak = os.environ["AWS_ACCESS_KEY_ID"]
        sk = os.environ["AWS_SECRET_ACCESS_KEY"]
    except KeyError as e:
        print("%s is not set" % e.args[0], file=sys.stderr)
        return 2

    client = Client(args.endpoint, args.bucket, args.region,
                    args.path_style, ak, sk, no_tls=args.no_tls)
    rc, msg = ensure(client)
    print("%s: %s" % (args.bucket, msg), file=sys.stderr if rc else sys.stdout)
    return rc


if __name__ == "__main__":
    sys.exit(main())
