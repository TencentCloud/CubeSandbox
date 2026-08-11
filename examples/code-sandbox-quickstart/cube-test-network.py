#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""End-to-end smoke test for L7 egress rules.

Drives the Python SDK to create sandboxes with various ``network.rules``
configurations and asserts the data plane (CubeEgress) actually enforces
them. The intent is operator-runnable verification, not pytest unit
testing — each scenario prints PASS/FAIL inline and the whole script
returns non-zero if any failed.

Coverage: every CubeEgress externally-observable capability gets at
least one positive and (where meaningful) one negative case:

  Match: host exact, host *.suffix wildcard, scheme http/https,
         method (single + any-of array), path exact, path "*" prefix.
  Action: allow=true (passthrough), allow=false (403),
          audit=metadata|none|full (smoke), inject[] with custom format,
          inject[] with default ${SECRET} format, multiple injects in
          one rule, strip-forged-header invariant, inject gate G1
          (https-only) drop on http.
  Decision semantics: first-match-wins both directions, default-deny
                     when no rule matches.
  Session longevity: one keep-alive HTTPS connection serves 1 request
                     per minute for 10 requests — proves the dataplane
                     session keeps steering an established flow to
                     CubeEgress even after the host's DNS-learned
                     allow_out_v3 /48 entry ages out. Runs twice: on
                     the default https port and on a custom port
                     (1012, where the legacy drain shim does NOT
                     rescue a lost session).

Restart-resilience: every case runs each of its probes TWICE — once
against the cube-egress that was already up when the sandbox was
created, then ``docker restart cube-egress`` and the SAME probes run
again against the fresh process. The case passes only if both passes
satisfy their predicates. This proves cube-egress correctly rebuilds
its in-memory ``policy_store`` from network-agent's
``/v1/policies/dump`` source-of-truth on cold start, with no
intervention needed (we DO NOT reinstall the policy ourselves between
the two passes).

Prerequisites:
  - CUBE_API_URL, CUBE_TEMPLATE_ID set
  - The template was built with --with-cube-ca=true (otherwise HTTPS
    interception will fail with self-signed-cert errors and the rule
    enforcement test results will be meaningless)
  - cube-egress is deployed on the node AS A DOCKER CONTAINER NAMED
    ``cube-egress`` (we drive ``docker restart cube-egress``); the
    runner needs permission to talk to the docker socket.
  - The sandbox image has Python with stdlib urllib (we deliberately
    avoid `requests` since it isn't on every base image)

Echo target: most cases use https://httpbingo.org/headers (and its
http:// counterpart) because it deterministically echoes the headers
the upstream server saw — the only public way to confirm an inject
actually flowed all the way to the origin without spending money on
a real authenticated API. We picked httpbingo over httpbin.org
specifically because httpbin's anycast often returns 503 from
mainland China (and its eu./www./nghttp2.org/httpbin mirrors are
network-blocked there); httpbingo (run on fly.io) responds reliably.
The wire shape differs slightly — httpbingo wraps every header value
in a single-element JSON array, e.g. ``"X-Api-Key": ["secret"]`` —
but our predicates use plain substring grep so the SECRET literal
still matches inside the array.

Usage:
    export CUBE_TEMPLATE_ID=tpl-d69160afa18a4c8b90b40b45
    export CUBE_API_URL=http://127.0.0.1:3000
    cd CubeSandbox/sdk/python && pip install -e .
    cd ../../tests
    python3 cube-test-network.py

Exit code: 0 on all-pass, non-zero on any failure or precondition miss.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import traceback
import urllib.request
from typing import Callable, List, Tuple
from env_utils import load_local_dotenv
load_local_dotenv()

# ── SDK import ───────────────────────────────────────────────────────────────
try:
    from cubesandbox import Sandbox, Rule, Match, Action, Inject
except ImportError as exc:
    print("[FATAL] cubesandbox SDK not importable:", exc)
    print("        Install with: cd sdk/python && pip install -e .")
    sys.exit(2)


# ── helpers ──────────────────────────────────────────────────────────────────

# Container + admin-port wiring. cube-egress's admin loopback listens on
# 127.0.0.1:9091 (declared in CubeEgress/nginx.conf; moved off :9090 by #1285
# because CubeProxy's plaintext gRPC serves :9090 on all-in-one hosts). The
# container is named ``cube-egress`` by the systemd unit
# ``cube-sandbox-cube-egress.service``.
CUBE_EGRESS_CONTAINER     = "cube-egress"
CUBE_EGRESS_HEALTH_URL    = "http://127.0.0.1:9091/admin/v1/health"
RESTART_BOOTSTRAP_TIMEOUT = 60   # seconds


def _check_env() -> None:
    """Bail out early if the SDK has nothing to talk to."""
    missing = [v for v in ("CUBE_API_URL", "CUBE_TEMPLATE_ID") if not os.environ.get(v)]
    if missing:
        print(f"[FATAL] missing env: {', '.join(missing)}")
        print("        export them before running this test")
        sys.exit(2)


def _restart_cube_egress() -> None:
    """``docker restart cube-egress`` and wait until it is serving again.

    "Serving" is observed via the admin /health endpoint:
      - it must answer at all (worker accepting on :9091)
      - bootstrap_status must reach "ready" (worker has fetched the
        policy dump from Cubelet's /v1/policies/dump)

    We deliberately do NOT re-install the test's policy ourselves —
    the whole point of the second pass is to verify cube-egress
    rebuilt its in-memory policy_store from Cubelet on cold
    start. If bootstrap landed on "skipped" (because
    CUBE_EGRESS_BOOTSTRAP_URL wasn't set on the container), the
    second pass would see policy_count=0 and most cases would 403,
    which is exactly the regression we want to surface.

    Raises RuntimeError if the container is not back to ready
    within RESTART_BOOTSTRAP_TIMEOUT — the case driver treats this
    as a fatal precondition miss for the post-restart pass.
    """
    print(f"  ── restarting {CUBE_EGRESS_CONTAINER} ──")
    t0 = time.monotonic()
    try:
        subprocess.run(
            ["docker", "restart", CUBE_EGRESS_CONTAINER],
            check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=30,
        )
    except subprocess.CalledProcessError as exc:
        raise RuntimeError(
            f"docker restart {CUBE_EGRESS_CONTAINER} failed: {exc.stderr.decode().strip()}"
        ) from exc
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError(
            f"docker restart {CUBE_EGRESS_CONTAINER} timed out after 30s"
        ) from exc

    deadline = time.monotonic() + RESTART_BOOTSTRAP_TIMEOUT
    last_err: str | None = None
    while time.monotonic() < deadline:
        try:
            resp = urllib.request.urlopen(CUBE_EGRESS_HEALTH_URL, timeout=2)
            body = json.loads(resp.read().decode())
            if body.get("bootstrap_status") == "ready":
                print(
                    f"  ── ready in {time.monotonic() - t0:.1f}s "
                    f"(policy_count={body.get('policy_count')}) ──"
                )
                # Belt-and-braces: give the listener a moment to settle the
                # transparent (TPROXY) sockets. Without this, the very first
                # post-restart probe occasionally lands on a half-initialised
                # worker and gets a connection reset before access_phase
                # runs. One second is empirically enough; this is far below
                # any reasonable per-case timeout budget.
                time.sleep(1.0)
                return
            last_err = f"bootstrap_status={body.get('bootstrap_status')}"
        except Exception as exc:  # noqa: BLE001
            last_err = type(exc).__name__ + ": " + str(exc)
        time.sleep(1.0)
    raise RuntimeError(
        f"{CUBE_EGRESS_CONTAINER} did not reach ready within "
        f"{RESTART_BOOTSTRAP_TIMEOUT}s (last={last_err})"
    )


def _http_probe(url: str, *, capture_body: bool = False, method: str = "GET",
                extra_headers: dict | None = None) -> str:
    """Sandbox-side helper: returns code that prints status + (optional) body.

    capture_body=True dumps the entire response body so we can grep echoed
    headers when the upstream is httpbingo.org/headers or /anything.
    extra_headers lets a test inject a *sandbox-side* header (the thing
    CubeEgress is supposed to clear when the rule plans an inject of the
    same name).
    """
    body_block = ""
    if capture_body:
        # Use unbounded resp.read() — chunked-transfer responses can
        # return 0 bytes from a size-bounded first read on Python's
        # urllib, which gave us empty BODY=... lines on httpbin in
        # earlier runs. We trust the upstream to be small (httpbin
        # /headers ≈ 200 B; /anything ≈ 1 KiB).
        # NOTE: lines must be indented 8 spaces so they stay INSIDE the
        # `with urlopen(...) as resp:` block — otherwise resp is closed
        # by the time we hit .read() and we get a deceptive zero-byte
        # body that silently fails the inject assertions.
        body_block = (
            "        raw_body = resp.read()\n"
            "        print(f'BODY_RAW_LEN={len(raw_body)}')\n"
            "        body = raw_body.decode('utf-8', 'replace')\n"
            "        print('BODY=' + body.replace(chr(10), ' '))\n"
        )
    headers_block = ""
    if extra_headers:
        # Render as a Python dict literal embedded in the sandbox script.
        items = ", ".join(f"{k!r}: {v!r}" for k, v in extra_headers.items())
        headers_block = f"_extra = {{{items}}}\n"
    else:
        headers_block = "_extra = {}\n"

    return f"""\
import urllib.request, urllib.error
{headers_block}_req = urllib.request.Request({url!r}, method={method!r})
for _k, _v in _extra.items():
    _req.add_header(_k, _v)
try:
    with urllib.request.urlopen(_req, timeout=30) as resp:
        print('STATUS=' + str(resp.status))
{body_block}except urllib.error.HTTPError as e:
    print('STATUS=' + str(e.code))
except Exception as e:
    print('ERROR=' + type(e).__name__ + ':' + str(e))
"""


def _logs_text(execution) -> str:
    """Flatten Execution stdout/stderr into one string for grep."""
    lines: List[str] = []
    if execution.logs and execution.logs.stdout:
        lines.extend(execution.logs.stdout)
    if execution.logs and execution.logs.stderr:
        lines.extend(execution.logs.stderr)
    if execution.text:
        lines.append(execution.text)
    return "\n".join(lines)


# ── twin-pass driver ─────────────────────────────────────────────────────────

# A "run" in a case is a triple (sub_label, sandbox-side code, predicate).
# Predicate takes the stdout string and returns True/False. The driver
# executes every run once, restarts cube-egress, then executes every run
# AGAIN inside the SAME sandbox, and returns True only if all 2N
# predicate evaluations hold.

Run = Tuple[str, str, Callable[[str], bool]]

# Markers indicating a transient upstream-hop failure inside cube-egress
# rather than a policy-decision result. We retry the probe (not the
# restart) when the predicate fails AND one of these markers is present.
#  - STATUS=502/503/504: nginx couldn't reach / TLS-verify the origin
#    (proxy_ssl_verify rejection, SYN drop, slow handshake > 10s
#    proxy_connect_timeout — all of which audit.lua flags as
#    upstream_unreachable_or_unverified). Especially common right
#    after a cold restart while DNS/TLS sessions are warming up.
#  - ERROR=URLError/TimeoutError/RemoteDisconnected: the sandbox-side
#    socket layer broke before/during the egress hop.
# We deliberately do NOT include STATUS=403 here — 403 is cube-egress'
# definitive policy decision, retrying it would only mask real
# regressions.
_TRANSIENT_MARKERS = (
    "STATUS=502",
    "STATUS=503",
    "STATUS=504",
    "ERROR=URLError",
    "ERROR=TimeoutError",
    "ERROR=RemoteDisconnected",
    "ERROR=ConnectionResetError",
    "ERROR=BadStatusLine",
)
_PROBE_MAX_ATTEMPTS = 3
_PROBE_RETRY_BACKOFF_SEC = 1.5


def _is_transient(out: str) -> bool:
    """Heuristic: did this probe fail with an upstream-hop error?

    Used to decide whether to retry the probe inside the SAME pass
    (no extra restart). Keep the marker list narrow so we don't
    paper over real policy regressions.
    """
    return any(m in out for m in _TRANSIENT_MARKERS)


def _exec_runs(sb: Sandbox, runs: List[Run], pass_label: str) -> bool:
    """Run every entry in `runs` against `sb` and return whether all passed.

    Each probe is retried up to ``_PROBE_MAX_ATTEMPTS`` times when its
    predicate fails AND the output looks like a transient upstream-hop
    failure (see ``_TRANSIENT_MARKERS``). Real policy regressions —
    e.g. STATUS=200 where the test expected 403 — fail on the first
    attempt without retry.

    Output for each entry is printed inline; the caller decides what
    to do with the boolean result.
    """
    all_ok = True
    for sub_label, code, pred in runs:
        out = ""
        ok = False
        for attempt in range(1, _PROBE_MAX_ATTEMPTS + 1):
            out = _logs_text(sb.run_code(code))
            ok = pred(out)
            if ok or not _is_transient(out) or attempt == _PROBE_MAX_ATTEMPTS:
                break
            # Transient AND we still have budget: pause briefly and retry.
            print(f"  [{pass_label} {sub_label}] transient (attempt {attempt}/"
                  f"{_PROBE_MAX_ATTEMPTS}); retrying after "
                  f"{_PROBE_RETRY_BACKOFF_SEC}s")
            time.sleep(_PROBE_RETRY_BACKOFF_SEC)

        snippet = out.strip().replace("\n", " | ")
        if len(snippet) > 400:
            snippet = snippet[:400] + "...(truncated)"
        all_ok = all_ok and ok
        marker = "ok" if ok else "FAIL"
        attempt_tag = f" (attempt {attempt})" if attempt > 1 else ""
        print(f"  [{pass_label} {sub_label}] {marker}{attempt_tag}: {snippet}")
    return all_ok


def _run_twice_with_restart(sb: Sandbox, runs: List[Run]) -> bool:
    """Drive a case: run all probes, restart cube-egress, run them again.

    Returns True iff every probe satisfied its predicate on BOTH passes.
    """
    pre_ok = _exec_runs(sb, runs, "pre")
    try:
        _restart_cube_egress()
    except RuntimeError as exc:
        print(f"  [restart] FAIL: {exc}")
        return False
    post_ok = _exec_runs(sb, runs, "post")
    return pre_ok and post_ok


# ── scenario builders ────────────────────────────────────────────────────────
#
# Each case constructs the sandbox, declares its `runs` list, and hands
# both to _run_twice_with_restart. The case body is therefore mostly
# data — match/action shape + per-probe predicate.

def _has_status(code: int) -> Callable[[str], bool]:
    """Predicate: stdout contains exactly STATUS=<code>.

    NOT a substring check — we use word boundaries via the literal
    'STATUS=<code>' marker our probe prints, which is unambiguous.
    """
    needle = f"STATUS={code}"
    return lambda out: needle in out


def _allow_then_403(code_allow: int = 200, code_deny: int = 403) -> Callable[[str], bool]:
    """Allow-and-no-403: a STATUS=<code_allow> line AND no STATUS=<code_deny>."""
    a, d = f"STATUS={code_allow}", f"STATUS={code_deny}"
    return lambda out: a in out and d not in out


def case_allow_specific_host() -> bool:
    """Allow rule on https://api.moonshot.cn/:443; expect a real HTTPS response."""
    rules = [
        Rule(
            name="allow_api.moonshot.cn",
            match=Match(scheme="https", host="api.moonshot.cn"),
            action=Action(allow=True, audit="metadata"),
        ),
    ]
    runs: List[Run] = [
        ("GET https://api.moonshot.cn",
         _http_probe("https://api.moonshot.cn"),
         _has_status(200)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_allow_suffix_host() -> bool:
    """Allow rule on https://*.moonshot.cn/:443; expect a real HTTPS response."""
    DENIED_CIDRS = [
        "0.0.0.0/0",  # block direct egress so traffic must use cube-egress tap
    ]
    rules = [
        Rule(
            name="allow_suffix",
            match=Match(host="*.moonshot.cn"),
            action=Action(allow=True, audit="metadata"),
        ),
    ]
    runs: List[Run] = [
        ("GET https://api.moonshot.cn",
         _http_probe("https://api.moonshot.cn"),
         _has_status(200)),
    ]
    with Sandbox.create(network={
        "allow_out": ["183.60.83.19"],
        "deny_out": DENIED_CIDRS,
        "rules": rules,
    }) as sb:
        return _run_twice_with_restart(sb, runs)


def case_deny_rule_returns_403() -> bool:
    """Explicit deny rule on a host; expect HTTP 403 from CubeEgress.

    Note: CubeEgress's deny path returns 403 without sending the request
    upstream. urllib raises HTTPError for non-2xx, which our probe maps
    back to STATUS=403.
    """
    rules = [
        Rule(
            name="block_evil",
            match=Match(scheme="https", host="api.moonshot.cn"),
            action=Action(allow=False),
        ),
    ]
    runs: List[Run] = [
        ("GET https://api.moonshot.cn (denied)",
         _http_probe("https://api.moonshot.cn"),
         _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_first_match_wins_order() -> bool:
    """Allow then deny, both matching same host.

    First-match-wins means the allow at index 0 takes effect; a STATUS
    other than 403 confirms the deny didn't override.
    """
    rules = [
        Rule(name="allow_first",
             match=Match(scheme="https", host="api.moonshot.cn"),
             action=Action(allow=True)),
        Rule(name="deny_second",
             match=Match(scheme="https", host="api.moonshot.cn"),
             action=Action(allow=False)),
    ]
    runs: List[Run] = [
        ("GET https://api.moonshot.cn (allow wins)",
         _http_probe("https://api.moonshot.cn"),
         _allow_then_403(200, 403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_first_match_wins_deny_before_allow() -> bool:
    """Deny then allow, both matching same host.

    The mirror of case_first_match_wins_order — proves the order matters
    in *both* directions (a buggy "last-match-wins" implementation would
    pass the allow-first case but fail this one).
    """
    rules = [
        Rule(name="deny_first",
             match=Match(scheme="https", host="api.moonshot.cn"),
             action=Action(allow=False)),
        Rule(name="allow_second",
             match=Match(scheme="https", host="api.moonshot.cn"),
             action=Action(allow=True)),
    ]
    runs: List[Run] = [
        ("GET https://api.moonshot.cn (deny wins)",
         _http_probe("https://api.moonshot.cn"),
         _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_default_deny_when_no_rule_matches() -> bool:
    """Policy installed with a host-scoped rule whose path doesn't match;
    request to a different path on the same host → 403.

    access_phase walks every rule; if none match, the request hits the
    bottom-of-policy default-deny branch (`no_rule_match`). This proves
    a non-empty policy doesn't accidentally fail open when match
    conditions narrow the rule down to a sub-slice of the host.

    We deliberately keep the rule's host and the request's host the same
    so the cubevs DNS allowlist (built from rule.match.host) still
    resolves the lookup — DNS is L3, default-deny is L7. The path
    mismatch is what triggers no_rule_match.
    """
    rules = [
        Rule(
            name="only_specific_path",
            match=Match(scheme="https", host="httpbingo.org",
                        path="/never-matches-this-exact-path"),
            action=Action(allow=True),
        ),
    ]
    runs: List[Run] = [
        ("GET https://httpbingo.org/headers (default-deny)",
         _http_probe("https://httpbingo.org/headers"),
         _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_method_filter_blocks_other_methods() -> bool:
    """Rule with method=['GET'] allows GET but blocks POST."""
    rules = [
        Rule(
            name="get_only",
            match=Match(scheme="https", host="httpbingo.org", method=["GET"]),
            action=Action(allow=True, audit="metadata"),
        ),
    ]
    runs: List[Run] = [
        ("GET /headers",  _http_probe("https://httpbingo.org/headers", method="GET"),
         _has_status(200)),
        ("POST /anything", _http_probe("https://httpbingo.org/anything", method="POST"),
         _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_method_any_of_array() -> bool:
    """Method any-of: ['GET','POST'] both allowed; PUT denied."""
    rules = [
        Rule(name="get_or_post",
             match=Match(scheme="https", host="httpbingo.org", method=["GET", "POST"]),
             action=Action(allow=True)),
    ]
    runs: List[Run] = [
        ("GET /anything",  _http_probe("https://httpbingo.org/anything", method="GET"),
         _has_status(200)),
        ("POST /anything", _http_probe("https://httpbingo.org/anything", method="POST"),
         _has_status(200)),
        ("PUT /anything",  _http_probe("https://httpbingo.org/anything", method="PUT"),
         _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_path_exact_match() -> bool:
    """Rule with path='/headers' allows /headers, denies /anything."""
    rules = [
        Rule(name="only_headers_path",
             match=Match(scheme="https", host="httpbingo.org", path="/headers"),
             action=Action(allow=True)),
    ]
    runs: List[Run] = [
        ("GET /headers (allow)",   _http_probe("https://httpbingo.org/headers"),  _has_status(200)),
        ("GET /anything (deny)",   _http_probe("https://httpbingo.org/anything"), _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_path_prefix_wildcard() -> bool:
    """Rule path='/anything*' allows /anything and /anything/foo; /headers denied.

    The trailing '*' is the only path wildcard CubeEgress supports
    (suffix-side; matches the way paths grow to the right).
    """
    rules = [
        Rule(name="anything_prefix",
             match=Match(scheme="https", host="httpbingo.org", path="/anything*"),
             action=Action(allow=True)),
    ]
    runs: List[Run] = [
        ("GET /anything",      _http_probe("https://httpbingo.org/anything"),     _has_status(200)),
        ("GET /anything/foo",  _http_probe("https://httpbingo.org/anything/foo"), _has_status(200)),
        ("GET /headers (deny)", _http_probe("https://httpbingo.org/headers"),     _has_status(403)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_scheme_http_allows_plain_http() -> bool:
    """scheme='http' rule allows the http:// listener (port 80 → 8080 TPROXY).

    Plain HTTP path is a separate code path from the SNI-driven HTTPS
    path; we want a smoke that confirms the access_phase fires and lets
    a matching rule through on the unencrypted listener.
    """
    rules = [
        Rule(name="http_only",
             match=Match(scheme="http", host="httpbingo.org"),
             action=Action(allow=True)),
    ]
    runs: List[Run] = [
        ("GET http://httpbingo.org/headers",
         _http_probe("http://httpbingo.org/headers"),
         _has_status(200)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_inject_default_format_renders_secret_only() -> bool:
    """Inject without ``format`` defaults to bare ``${SECRET}``.

    Verify by greping the echoed-headers body returned from
    https://httpbingo.org/headers — the X-Api-Key value should be the
    raw secret with no Bearer/literal prefix.
    """
    SECRET = "cube-test-DEFAULT-FMT-eb91"
    rules = [
        Rule(
            name="inject_default_fmt",
            match=Match(scheme="https", host="httpbingo.org"),
            action=Action(allow=True, audit="metadata",
                          inject=[Inject(header="X-Api-Key", secret=SECRET)]),
        ),
    ]
    pred = lambda out: ("STATUS=200" in out) and (SECRET in out)  # noqa: E731
    runs: List[Run] = [
        ("GET /headers w/ X-Api-Key inject",
         _http_probe("https://httpbingo.org/headers", capture_body=True),
         pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_inject_multiple_headers_in_one_rule() -> bool:
    """Two inject entries on the same rule both flow upstream.

    Validates apply_injects walks the whole inject_list (not just [0]).
    """
    SECRET_A = "cube-test-A-77f1"
    SECRET_B = "cube-test-B-2c0d"
    rules = [
        Rule(
            name="inject_pair",
            match=Match(scheme="https", host="httpbingo.org"),
            action=Action(
                allow=True,
                inject=[
                    Inject(header="X-Cube-A", secret=SECRET_A),
                    Inject(header="X-Cube-B", secret=SECRET_B,
                           format="prefix-${SECRET}"),
                ],
            ),
        ),
    ]
    def pred(out: str) -> bool:
        return ("STATUS=200" in out
                and SECRET_A in out
                and ("prefix-" + SECRET_B) in out)
    runs: List[Run] = [
        ("GET /headers w/ two injects",
         _http_probe("https://httpbingo.org/headers", capture_body=True),
         pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_inject_strips_forged_sandbox_header() -> bool:
    """Inject MUST overwrite (not append-to) a sandbox-supplied header.

    The sandbox sends ``Authorization: forged-by-sandbox``; the rule
    injects its own Authorization header with a known secret. After
    egress, httpbingo.org should echo the *injected* value, not the
    forged one. This is the documented race-protection invariant in
    access_phase.allow():

        > we ALWAYS clear that header from the sandbox-provided
        > request first, whether or not the gates pass
    """
    SECRET = "cube-test-OVERWRITE-9e1a"
    FORGED = "Bearer forged-by-sandbox"
    rules = [
        Rule(
            name="overwrite_auth",
            match=Match(scheme="https", host="httpbingo.org"),
            action=Action(
                allow=True,
                inject=[Inject(header="Authorization", secret=SECRET,
                               format="Bearer ${SECRET}")],
            ),
        ),
    ]
    def pred(out: str) -> bool:
        return ("STATUS=200" in out
                and SECRET in out
                and "forged-by-sandbox" not in out)
    code = _http_probe(
        "https://httpbingo.org/headers",
        capture_body=True,
        extra_headers={"Authorization": FORGED},
    )
    runs: List[Run] = [
        ("GET /headers w/ forged Authorization", code, pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_inject_dropped_on_http_scheme_request_proceeds() -> bool:
    """HTTP inject applies (CubeEgress no longer blocks inject on http).

    Historic behavior: G1 gate rejected inject on non-HTTPS traffic
    ("https-only inject"). Current behavior (see
    CubeEgress/lua/access_phase.lua:inject_gates comment on G1): operators
    may legitimately need to inject credentials into plaintext traffic on
    trusted networks (e.g. intra-cluster 80), so the http scheme is
    permitted. This test guards that policy change by asserting the
    injected header DOES appear in the upstream echo — the opposite of
    the pre-relaxation expectation.

    If G1 is ever re-tightened to https-only, flip the predicate back to
    ``SECRET not in out`` and rename this case.
    """
    SECRET = "cube-test-http-inject-44ad"
    rules = [
        Rule(
            name="http_with_inject",
            match=Match(scheme="http", host="httpbingo.org"),
            action=Action(
                allow=True,
                inject=[Inject(header="X-Should-Appear", secret=SECRET)],
            ),
        ),
    ]
    def pred(out: str) -> bool:
        return "STATUS=200" in out and SECRET in out
    runs: List[Run] = [
        ("GET http://httpbingo.org/headers (http inject)",
         _http_probe("http://httpbingo.org/headers", capture_body=True),
         pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def case_audit_none_still_allows() -> bool:
    """audit='none' is a valid AuditLevel and does not block traffic."""
    rules = [
        Rule(name="silent_audit",
             match=Match(scheme="https", host="httpbingo.org"),
             action=Action(allow=True, audit="none")),
    ]
    runs: List[Run] = [
        ("GET https://httpbingo.org/headers",
         _http_probe("https://httpbingo.org/headers"),
         _has_status(200)),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


def _longconn_https_probe(host: str, port: int, path: str = "/headers") -> str:
    """Sandbox-side code: 10 HTTPS requests, 1 per minute, on ONE connection.

    Prints one ``REQ i STATUS=... SAMECONN=...`` line per request and a
    final LONGCONN_ALL_OK / LONGCONN_FAILED marker. "Same connection" is
    pinned via the socket's local address after every response — a
    transparent reconnect changes the local port (or nulls the socket
    after Connection: close) and surfaces as SAMECONN=False.
    CubeEgress's downstream keepalive_timeout is 65s
    (CubeEgress/nginx.conf:105), so the 60s period stays alive. A
    User-Agent is always sent: httpbingo.org (fly.io) rejects UA-less
    requests with an empty-body 402.
    """
    return f"""\
import http.client, time

HOST, PORT, PATH = {host!r}, {port}, {path!r}
N, INTERVAL = 10, 60
UA = {{"User-Agent": "cube-longconn-probe/1.0"}}

conn = http.client.HTTPSConnection(HOST, PORT, timeout=30)

# Warm-up on the SAME connection: the per-sandbox policy push to
# cube-egress is async, so ride out the first seconds of default-deny
# 403s before starting the official loop (connection stays keep-alive).
t_end = time.time() + 60
while True:
    try:
        conn.request("GET", PATH, headers=UA)
        r = conn.getresponse()
        r.read()
        if r.status != 403:
            break
    except Exception:
        pass
    if time.time() > t_end:
        print("WARMUP_FAILED", flush=True)
        raise SystemExit(1)
    time.sleep(2)

local = None
ok = True
for i in range(1, N + 1):
    t0 = time.time()
    try:
        conn.request("GET", PATH, headers=UA)
        resp = conn.getresponse()
        resp.read()
        status, will_close = resp.status, resp.will_close
    except Exception as e:
        print(f"REQ {{i}} EXC={{type(e).__name__}}:{{e}}", flush=True)
        ok = False
        break
    cur = conn.sock.getsockname() if conn.sock else None
    if local is None:
        local = cur
    same = (cur == local) and not will_close
    print(f"REQ {{i}} STATUS={{status}} SAMECONN={{same}} LOCAL={{cur}}", flush=True)
    ok = ok and (status == 200) and same
    if i < N:
        time.sleep(max(INTERVAL - (time.time() - t0), 1))
print("LONGCONN_ALL_OK" if ok else "LONGCONN_FAILED", flush=True)
"""


def _longconn_pred(out: str) -> bool:
    """Predicate for _longconn_https_probe: 10x200, same socket throughout."""
    return ("LONGCONN_ALL_OK" in out
            and "SAMECONN=False" not in out
            and out.count("STATUS=200") == 10)


def case_long_lived_https_connection_10x1min() -> bool:
    """One keep-alive HTTPS connection, 1 request/min x 10, all must pass.

    Exercises the dataplane session-hit path over ~9 minutes: the
    DNS-learned allow_out_v3 /48 entry for the host ages out mid-test
    (resolver TTLs are typically <= 300s), while the established
    connection's egress_sessions entry (no TTL of its own; ESTABLISHED
    timeout ~3h) must keep steering packets to CubeEgress. If any
    request fails — e.g. the dataplane re-classified the flow after
    /48 aging and rejected it — the case fails.

    Deliberately runs WITHOUT the cube-egress restart twin-pass:
    restarting the proxy would sever the very connection under test.
    This case targets dataplane session longevity, not policy rebuild.
    """
    rules = [
        Rule(name="longconn",
             match=Match(scheme="https", host="httpbingo.org"),
             action=Action(allow=True, audit="metadata")),
    ]
    runs: List[Run] = [
        ("https keep-alive 1 req/min x10 on one socket",
         _longconn_https_probe("httpbingo.org", 443),
         _longconn_pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _exec_runs(sb, runs, "longconn")


def case_long_lived_https_custom_port_10x1min() -> bool:
    """Same longevity test against a NON-standard destination port (1012).

    tls-v1-2.badssl.com:1012 is a public HTTPS endpoint off 443; the
    rule pins scheme=https + port=1012, so flow classification, the
    DNS-learned (ip, port)/48 entry aging, and session steering all
    exercise the custom-port dataplane. Unlike 80/443 flows, a lost
    session here is NOT rescued by the legacy drain shim in do_tcp_nat
    (custom ports are intentionally excluded), so this case is the
    stronger proof that the session entry really persists for the
    whole ~9 minutes.
    """
    rules = [
        Rule(name="longconn_custom_port",
             match=Match(scheme="https", host="tls-v1-2.badssl.com", port=1012),
             action=Action(allow=True, audit="metadata")),
    ]
    runs: List[Run] = [
        ("https keep-alive 1 req/min x10 on custom port 1012",
         _longconn_https_probe("tls-v1-2.badssl.com", 1012, "/"),
         _longconn_pred),
    ]
    with Sandbox.create(network={"rules": rules}) as sb:
        return _exec_runs(sb, runs, "longconn")


def case_secret_injection_via_header_for_moonshot() -> bool:
    """Optionally verify credential injection against an authenticated API."""
    api_key = os.environ.get("MOONSHOT_API_KEY")
    if not api_key:
        print("SKIP: MOONSHOT_API_KEY is not set")
        return True

    rules = [
        Rule(
            name="moonshot",
            match=Match(scheme="https", host="*.moonshot.cn", port=443),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[Inject(
                    header="Authorization",
                    secret=api_key,
                    format="Bearer ${SECRET}",
                )],
            ),
        ),
    ]
    DENIED_CIDRS = [
        "0.0.0.0/0",  # block direct egress so traffic must use cube-egress tap
    ]
    python_code = """
import json
import urllib.request

url = "https://api.moonshot.cn/v1/chat/completions"
headers = {"Content-Type": "application/json"}

payload = {
    "messages": [
        {"content": "Hello! Who Are You？", "role": "user"}
    ],
    "model": "kimi-k2.5",
    "stream": False
}

data = json.dumps(payload).encode("utf-8")
req = urllib.request.Request(url, data=data, headers=headers, method="POST")

try:
    with urllib.request.urlopen(req, timeout=60) as response:
        response_body = response.read().decode("utf-8")
        print("Status code:", response.getcode())
        print("Response:", response_body)
except urllib.error.HTTPError as e:
    print("HTTP Error:", e.code, e.reason)
    print("Response body:", e.read().decode())
"""
    def pred(out: str) -> bool:
        return "Status code: 200" in out and "completion_tokens" in out
    runs: List[Run] = [
        ("POST /v1/chat/completions w/ inject", python_code, pred),
    ]
    with Sandbox.create(network={"deny_out": DENIED_CIDRS, "rules": rules}) as sb:
        return _run_twice_with_restart(sb, runs)


# ── runner ───────────────────────────────────────────────────────────────────

CASES: List[Tuple[str, Callable[[], bool]]] = [
    #("allow rule on https host returns real upstream response",       case_allow_specific_host),
    #("allow * on https suffix host returns real upstream response",   case_allow_suffix_host),
    #("deny rule returns 403 without hitting upstream",                case_deny_rule_returns_403),
    #("first-match-wins picks the earlier allow over later deny",      case_first_match_wins_order),
    #("first-match-wins picks the earlier deny over later allow",      case_first_match_wins_deny_before_allow),
    #("non-empty policy default-denies hosts not covered by any rule", case_default_deny_when_no_rule_matches),
    #("method filter allows GET and blocks POST on same host",         case_method_filter_blocks_other_methods),
    #("method any-of array allows multiple methods, denies others",    case_method_any_of_array),
    #("path exact match allows /headers and denies /anything",         case_path_exact_match),
    #("path '*' suffix wildcard allows whole subtree",                 case_path_prefix_wildcard),
    #("scheme=http rule allows plain-HTTP traffic",                    case_scheme_http_allows_plain_http),
    #("inject default format renders the bare secret",                 case_inject_default_format_renders_secret_only),
    #("multiple inject entries in one rule all reach upstream",        case_inject_multiple_headers_in_one_rule),
    #("inject overwrites a sandbox-forged Authorization header",       case_inject_strips_forged_sandbox_header),
    #("inject applies on http scheme (G1 no longer https-only)",       case_inject_dropped_on_http_scheme_request_proceeds),
    #("audit='none' rule still allows traffic",                        case_audit_none_still_allows),
    #("long-lived https connection serves 1 req/min x10 on one socket", case_long_lived_https_connection_10x1min),
    ("long-lived https connection on custom port 1012 (1 req/min x10)", case_long_lived_https_custom_port_10x1min),
    #("inject Authorization: Bearer <secret> reaches moonshot upstream", case_secret_injection_via_header_for_moonshot),
]


def main() -> int:
    _check_env()
    print(f"CUBE_API_URL     = {os.environ.get('CUBE_API_URL')}")
    print(f"CUBE_TEMPLATE_ID = {os.environ.get('CUBE_TEMPLATE_ID')}")
    print(f"Restart container = {CUBE_EGRESS_CONTAINER}")
    print()

    passed = failed = 0
    for label, fn in CASES:
        print(f"== {label} ==")
        try:
            ok = fn()
        except Exception:
            print("  EXCEPTION:")
            traceback.print_exc()
            ok = False
        if ok:
            print("  RESULT: PASS\n")
            passed += 1
        else:
            print("  RESULT: FAIL\n")
            failed += 1

    print(f"summary: {passed} passed, {failed} failed (of {len(CASES)})")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
