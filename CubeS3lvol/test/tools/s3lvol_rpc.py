#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  One JSON-RPC call to a running s3lvol target.
#
#  === Why not spdk/scripts/rpc.py ===
#
#  rpc.py only knows the methods it has Python wrappers for. The s3lvol methods
#  (rcow_create_lvstore, rcow_attach_lvstore, rcow_checkpoint_lvstore, ...) live
#  in this repo, not in SPDK, so rpc.py rejects them outright. Every test script
#  therefore needs a way to send a raw request, and this is it -- previously
#  copied into each script, which is how the two copies drifted to different
#  socket timeouts.
#
#  Also useful by hand when reproducing a failure against a target that a test
#  left running:
#
#    test/tools/s3lvol_rpc.py rcow_get_lvstores
#    test/tools/s3lvol_rpc.py rcow_checkpoint_lvstore '{"lvs_name":"dpvs"}'
#
#  === Output contract, relied on by the shell callers ===
#
#    success -> the "result" member, as JSON, on stdout; exit 0
#    error   -> the "error" member, as JSON, on stderr; exit 1
#
#  So `out=$(s3lvol_rpc.py ...) || handle_failure` works, and a failed call never
#  puts anything on stdout that a caller could mistake for a result.
#
#  === The {bool_value, string_value} envelope ===
#
#  Every RPC an external caller uses answers with the same two fields, whether it
#  worked or not (see the header of vbdev_s3lvol_rpc.c):
#
#    {"bool_value": true,  "string_value": "vol0"}
#    {"bool_value": false, "string_value": "lvol 'vol0' not found"}
#
#  Both are JSON-RPC *successes*, so the transport no longer distinguishes them
#  and the contract above would report every failure as exit 0. This unwraps the
#  envelope instead:
#
#    bool_value true  -> string_value on stdout, exit 0
#    bool_value false -> string_value on stderr, exit 1
#
#  which restores the contract, and does it in one place rather than in each of
#  the ~200 call sites across the test suite and scripts/.
#
#  Printing string_value *verbatim* is what makes the unwrapping transparent: the
#  RPCs with more than a name to report put a serialised JSON document in there
#  (rcow_get_bdev, rcow_get_decouple, rcow_get_imports, rcow_active_bdev), so
#  stdout ends up carrying exactly the object or array those RPCs used to return,
#  and callers that parse it need no change at all.
#
#  --raw turns this off, which is what to use when the question is what the target
#  actually put on the wire.

import argparse
import json
import socket
import sys


def main():
    p = argparse.ArgumentParser(description="send one JSON-RPC request to an "
                                            "s3lvol target")
    p.add_argument("method")
    p.add_argument("params", nargs="?", default="",
                   help="request parameters as a JSON object; omit or pass an "
                        "empty string for none")
    p.add_argument("--sock", default="/var/run/s3lvol.sock",
                   help="RPC unix socket (default: %(default)s)")
    # Generous by default: rcow_create_lvstore formats the local device
    # and talks to S3, and an attach replays the journal and the WAL before it
    # answers. A tight timeout here reports a hang that is really just slow.
    p.add_argument("--timeout", type=float, default=300.0,
                   help="socket timeout in seconds (default: %(default)s)")
    p.add_argument("--raw", action="store_true",
                   help="print the result member as it arrived, without "
                        "unwrapping a {bool_value, string_value} envelope")
    args = p.parse_args()

    req = {"jsonrpc": "2.0", "id": 1, "method": args.method}
    if args.params:
        try:
            req["params"] = json.loads(args.params)
        except ValueError as e:
            print("params is not valid JSON: %s" % e, file=sys.stderr)
            return 1

    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(args.timeout)
    try:
        s.connect(args.sock)
    except OSError as e:
        print("cannot connect to %s: %s" % (args.sock, e), file=sys.stderr)
        return 1
    s.sendall(json.dumps(req).encode())

    # The reply can arrive in pieces, and its length is not announced, so the
    # only way to know it is complete is that it parses.
    buf = b""
    resp = None
    while True:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            print("no reply within %gs; %d bytes received"
                  % (args.timeout, len(buf)), file=sys.stderr)
            s.close()
            return 1
        if not chunk:
            break
        buf += chunk
        try:
            resp = json.loads(buf.decode())
            break
        except ValueError:
            continue
    s.close()

    # The socket closing before a complete reply arrived means the target died
    # mid-request. Say that, rather than dying on an unbound name -- the first
    # live run of the dataplane test reported "NameError: resp is not defined"
    # for what was actually a segfault on the other end.
    if resp is None:
        print("target closed the connection without replying (it probably "
              "died); %d bytes received" % len(buf), file=sys.stderr)
        return 1

    if "error" in resp:
        print(json.dumps(resp["error"]), file=sys.stderr)
        return 1

    result = resp.get("result")

    # The envelope, recognised by its exact shape: an object with those two keys
    # and nothing else. Being that strict matters -- an RPC that happened to
    # report a bool_value among other fields would otherwise have the rest of its
    # answer thrown away.
    if (not args.raw and isinstance(result, dict)
            and set(result) == {"bool_value", "string_value"}
            and isinstance(result["bool_value"], bool)):
        text = result["string_value"]
        if result["bool_value"]:
            print(text)
            return 0
        print(text, file=sys.stderr)
        return 1

    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
