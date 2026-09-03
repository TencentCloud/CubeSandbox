#!/usr/bin/env python3
# 探针工具：往 S3 写一个 export lease 对象，模拟另一节点的 importer 续约。
# 复用 s3_prefix_rm.py 的 stdlib SigV4 签名（测试机没有 boto3）。
import datetime
import hashlib
import hmac
import http.client
import json
import os
import sys
import time

ap = sys.argv
if len(ap) < 6:
    sys.exit("usage: put_lease.py <endpoint> <bucket> <region> <key> <renew_s> [age_s]")
endpoint, bucket, region, key, renew_s = ap[1], ap[2], ap[3], ap[4], int(ap[5])
age = int(ap[6]) if len(ap) > 6 else 0

ak = os.environ["AWS_ACCESS_KEY_ID"]
sk = os.environ["AWS_SECRET_ACCESS_KEY"]
host = "%s.%s" % (bucket, endpoint)

body = json.dumps({"importer_id": "probe",
                   "updated_at": int(time.time()) - age,
                   "renew_s": renew_s}).encode()
payload_sha = hashlib.sha256(body).hexdigest()

now = datetime.datetime.now(datetime.timezone.utc)
amzdate = now.strftime("%Y%m%dT%H%M%SZ")
datestamp = now.strftime("%Y%m%d")
path = "/" + key

canonical_headers = ("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n"
                     % (host, payload_sha, amzdate))
signed_headers = "host;x-amz-content-sha256;x-amz-date"
canonical_request = "\n".join(["PUT", path, "", canonical_headers,
                               signed_headers, payload_sha])
scope = "%s/%s/s3/aws4_request" % (datestamp, region)
to_sign = "\n".join(["AWS4-HMAC-SHA256", amzdate, scope,
                     hashlib.sha256(canonical_request.encode()).hexdigest()])


def sign(k, m):
    return hmac.new(k, m.encode(), hashlib.sha256).digest()


k = sign(("AWS4" + sk).encode(), datestamp)
k = sign(k, region)
k = sign(k, "s3")
k = sign(k, "aws4_request")
signature = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
auth = ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s"
        % (ak, scope, signed_headers, signature))

conn = http.client.HTTPSConnection(host, timeout=60)
conn.request("PUT", path, body=body, headers={
    "Host": host, "x-amz-date": amzdate,
    "x-amz-content-sha256": payload_sha,
    "Authorization": auth, "Content-Length": str(len(body)),
})
r = conn.getresponse()
print("PUT %s -> HTTP %d" % (key, r.status))
r.read()
conn.close()
sys.exit(0 if r.status in (200, 204) else 1)
