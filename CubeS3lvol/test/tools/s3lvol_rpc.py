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
#
#  === Retrying failed snapshot deletes: --retry-pending ===
#
#  Deleting a snapshot that is pinned (an export names it, it has clones, or a
#  decouple is running) is refused, and the refusal records a pending-delete
#  mark for the snapshot. The mark is the *only* record that a delete was asked
#  for: from the target's side every delete arrives as the same RPC, so there
#  is no way to tell a user's delete from an automation's. The mark therefore
#  is what "the user asked for this delete" means.
#
#    test/tools/s3lvol_rpc.py --retry-pending
#
#  reads rcow_get_lvstores, finds every snapshot that carries the mark AND has
#  become deletable since the refusal (deletable is YES), and calls
#  rcow_delete_lvol for each, passing the snapshot's uuid along with its name.
#  A snapshot without the mark is never touched, so snapshots a test or another
#  node created, or that were never deleted on purpose, are left alone. Each
#  successful delete clears the mark on the target; a delete that is refused
#  again is reported and does not stop the others. Exit status is 0 only if
#  every marked-and-deletable snapshot went.
#
#  What this is not:
#
#    * There is no automatic retry. Nothing on the target polls the marks; this
#      command is the only thing that acts on them, and it has to be run.
#    * The marks live in the target's memory only. A restart loses them, and a
#      delete refused before the restart is then indistinguishable from one that
#      was never asked for.
#    * There is no way to cancel a mark other than completing the delete. A
#      refused delete stays recorded for as long as the target runs and its
#      lvstore stays attached (an unload drops that lvstore's marks).
#    * The cluster deployment does not run this: Cubelet's own delete path
#      (S3Cow.DeleteByKind) still treats a refused snapshot delete as success.
#      This is an operator tool for the node, not a fix for that.

import argparse
import json
import socket
import sys


def _fmt_size(clusters, cluster_size):
    if not clusters:
        return "-"
    b = float(clusters) * cluster_size
    units = ("B", "KiB", "MiB", "GiB", "TiB")
    for i, unit in enumerate(units):
        if b < 1024.0 or i == len(units) - 1:
            return "%d %s" % (b, unit) if unit == "B" else "%.1f %s" % (b, unit)
        b /= 1024.0


def fmt_ls_table(result):
    """rcow_get_lvstores -> an ls-style table, one lvol per line."""
    if not isinstance(result, list):
        return json.dumps(result)
    blocks = []
    for lvs in result:
        if not isinstance(lvs, dict):
            continue
        lvols = lvs.get("lvols") or []
        cs = lvs.get("cluster_size") or 1048576
        lines = ["LVS %s  (total %d / free %d clusters, %d lvols)" % (
            lvs.get("lvs_name", "?"), lvs.get("total_clusters", 0),
            lvs.get("free_clusters", 0), len(lvols))]
        rows = [(l.get("name", ""),
                 "snapshot" if l.get("is_snapshot") else "lvol",
                 _fmt_size(l.get("total_clusters", 0), cs),
                 str(l.get("allocated_clusters", 0)),
                 l.get("export_status", ""),
                 l.get("deletable", ""),
                 "Y" if l.get("delete_pending") else "-",
                 l.get("bdev_name", "")) for l in lvols]
        w = max((len(r[0]) for r in rows), default=8)
        fmt = "%-*s  %-9s %-10s %-8s %-6s %-4s %-5s  %s"
        hdr = fmt % (w, "NAME", "TYPE", "SIZE", "ALLOC", "EXPORT", "DEL", "PEND", "BDEV")
        lines.append(hdr)
        lines.append("-" * len(hdr))
        for r in rows:
            lines.append(fmt % (w, r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]))
        blocks.append("\n".join(lines))
    return "\n".join(blocks)


def _send(sock_path, timeout, method, params=None, raw=False):
    """One JSON-RPC request.

    Returns (True, value, envelope) where value is the result with a
    {bool_value, string_value} envelope unwrapped (string_value, verbatim), or
    (False, text, False) where text says what went wrong.
    """
    req = {"jsonrpc": "2.0", "id": 1, "method": method}
    if params is not None:
        req["params"] = params

    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect(sock_path)
    except OSError as e:
        return False, "cannot connect to %s: %s" % (sock_path, e), False
    s.sendall(json.dumps(req).encode())

    # The reply can arrive in pieces, and its length is not announced, so the
    # only way to know it is complete is that it parses.
    buf = b""
    resp = None
    while True:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            s.close()
            return False, "no reply within %gs; %d bytes received" % (
                timeout, len(buf)), False
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
        return False, ("target closed the connection without replying (it "
                       "probably died); %d bytes received" % len(buf)), False

    if "error" in resp:
        return False, json.dumps(resp["error"]), False

    result = resp.get("result")

    # The envelope, recognised by its exact shape: an object with those two keys
    # and nothing else. Being that strict matters -- an RPC that happened to
    # report a bool_value among other fields would otherwise have the rest of its
    # answer thrown away.
    if (not raw and isinstance(result, dict)
            and set(result) == {"bool_value", "string_value"}
            and isinstance(result["bool_value"], bool)):
        text = result["string_value"]
        if result["bool_value"]:
            return True, text, True
        return False, text, True

    return True, result, False


def retry_pending_deletes(args):
    """Delete every snapshot that carries a pending-delete mark and is
    deletable now. See the header for what the mark means and why it is
    trusted as the record of a user-asked delete."""
    ok, result, _ = _send(args.sock, args.timeout, "rcow_get_lvstores")
    if not ok:
        print("rcow_get_lvstores failed: %s" % result, file=sys.stderr)
        return 1
    if not isinstance(result, list):
        print("unexpected rcow_get_lvstores result: %r" % (result,),
              file=sys.stderr)
        return 1

    targets = []
    for lvs in result:
        if not isinstance(lvs, dict):
            continue
        lvs_uuid = lvs.get("uuid", "")
        for l in lvs.get("lvols") or []:
            # delete_pending is the record that a delete was asked for and
            # refused; deletable=YES means the blocker has cleared. Together
            # they are exactly "a user asked, it failed, retry now".
            #
            # delete_pending alone establishes snapshot-ness: the target only
            # ever records a mark for a snapshot. is_snapshot is not required
            # on top of it -- and must not be, since a snapshot deactivated
            # while it waited for its blocker to clear would then never be
            # retried.
            if l.get("delete_pending") and l.get("deletable") == "YES":
                targets.append((l.get("name", ""), l.get("uuid", ""),
                                lvs_uuid))

    if not targets:
        print("no pending snapshot deletes to retry")
        return 0

    done = 0
    for name, lvol_uuid, lvs_uuid in targets:
        # The uuids come from the same rcow_get_lvstores answer the mark was
        # read from, and the target refuses if the name has since moved to a
        # different object. Deleting by name alone would let a snapshot that
        # was recreated under the same name be deleted by a retry meant for
        # its predecessor.
        params = {"lvol_name": name}
        if lvol_uuid:
            params["lvol_uuid"] = lvol_uuid
        if lvs_uuid:
            params["lvs_uuid"] = lvs_uuid
        ok, text, _ = _send(args.sock, args.timeout, "rcow_delete_lvol",
                            params)
        if ok:
            done += 1
            print("deleted %s" % name)
        else:
            print("failed to delete %s: %s" % (name, text), file=sys.stderr)
    print("%d of %d pending snapshot delete(s) completed" % (done, len(targets)))
    return 0 if done == len(targets) else 1


def main():
    p = argparse.ArgumentParser(description="send one JSON-RPC request to an "
                                            "s3lvol target")
    p.add_argument("method", nargs="?",
                   help="method to call; not needed with --retry-pending")
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
    p.add_argument("--ls", action="store_true",
                   help="format rcow_get_lvstores output as an ls-style "
                        "table (NAME/SIZE/ALLOC/EXPORT/DEL/BDEV)")
    p.add_argument("--retry-pending", action="store_true",
                   help="delete every snapshot that carries a pending-delete "
                        "mark and has become deletable; takes no method")
    args = p.parse_args()

    if args.retry_pending:
        if args.method:
            print("--retry-pending takes no method", file=sys.stderr)
            return 2
        return retry_pending_deletes(args)

    if not args.method:
        p.print_usage(sys.stderr)
        print("error: a method is required (or use --retry-pending)",
              file=sys.stderr)
        return 2

    if args.params:
        try:
            params = json.loads(args.params)
        except ValueError as e:
            print("params is not valid JSON: %s" % e, file=sys.stderr)
            return 1
    else:
        params = None

    ok, result, envelope = _send(args.sock, args.timeout, args.method,
                                 params, args.raw)
    if not ok:
        print(result, file=sys.stderr)
        return 1

    if args.ls and args.method == "rcow_get_lvstores":
        print(fmt_ls_table(result))
    elif envelope:
        print(result)
    else:
        print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
