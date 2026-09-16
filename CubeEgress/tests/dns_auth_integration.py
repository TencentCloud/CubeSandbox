#!/usr/bin/env python3
"""Exercise production access/audit/proxy locations with real DNS and TLS.

Requires Docker, Python 3 and openssl on the host. No target environment is
modified. Policy storage and certificate issuance are fixture adapters; packet
steering is replaced by direct sockets preserving distinct destination IPs.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import http.client
import http.server
import json
import os
from pathlib import Path
import socket
import socketserver
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import uuid


def block(text, marker):
    start = text.index(marker)
    opening = text.index("{", start)
    depth = 1
    end = opening + 1
    while depth:
        depth += (text[end] == "{") - (text[end] == "}")
        end += 1
    return text[start:end]


def fixture_config(source):
    config = source.replace("user cube-proxy cube-proxy;", "user root;")
    config = config.replace("worker_processes 2;", "worker_processes 1;")
    config = config.replace("http {", "http {\n    lua_check_client_abort on;", 1)
    config = config.replace("/var/run/openresty", "/tmp")
    config = config.replace("/usr/local/openresty/nginx/lua/?.lua", "/fixture/?.lua;/repo/lua/?.lua")
    config = config.replace("192.168.0.1:8080 transparent reuseport", "0.0.0.0:80")
    config = config.replace("192.168.0.1:8443 ssl transparent reuseport", "0.0.0.0:443 ssl")
    config = config.replace("http://$server_addr:$server_port", "http://$server_addr:18080")
    config = config.replace("https://$server_addr:$server_port", "https://$server_addr:18443")
    config = config.replace("/etc/cube/ca/placeholder.crt", "/fixture/cert.pem")
    config = config.replace("/etc/cube/ca/placeholder.key", "/fixture/key.pem")
    config = config.replace("/etc/ssl/certs/ca-certificates.crt", "/fixture/cert.pem")
    config = config.replace(block(config, "init_by_lua_block"), '''init_by_lua_block {
        audit = require("audit")
        audit.bootstrap({file_path = "/tmp/audit.jsonl"})
        package.preload["policy"] = function()
            return {get = function()
                return require("cjson.safe").decode(ngx.shared.policy_store:get("fixture") or "null")
            end}
        end
    }''')
    config = config.replace(block(config, "init_worker_by_lua_block"), '''init_worker_by_lua_block {
        audit.init_worker()
        ngx.shared.meta_store:set("bootstrap_status", "ready")
    }''')
    config = config.replace(block(config, "ssl_certificate_by_lua_block"), "")
    config = config.replace('require("admin").dispatch()', '''ngx.req.read_body()
                ngx.shared.policy_store:set("fixture", ngx.req.get_body_data())
                ngx.say("ok")''')
    return config


def host_run(args):
    repo = Path(__file__).resolve().parents[1]
    name = "egress-dns-test-" + uuid.uuid4().hex[:10]
    with tempfile.TemporaryDirectory(prefix="egress-dns-") as directory:
        fixture = Path(directory)
        fixture.chmod(0o755)
        (fixture / "hosts").write_text("127.0.0.1 localhost\n127.0.0.3 allowed.example.test\n")
        (fixture / "nginx.conf").write_text(fixture_config((repo / "nginx.conf").read_text()))
        if args.baseline_ref:
            old = subprocess.check_output(["git", "show", args.baseline_ref + ":CubeEgress/lua/access_phase.lua"], cwd=repo)
            (fixture / "access_phase.lua").write_bytes(old)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                        "-days", "1", "-subj", "/CN=allowed.example.test",
                        "-addext", "subjectAltName=DNS:*.example.test",
                        "-keyout", str(fixture / "key.pem"), "-out", str(fixture / "cert.pem")],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        mount = ["-v", str(repo) + ":/repo:ro", "-v", directory + ":/fixture:ro"]
        try:
            subprocess.run(["docker", "run", "-d", "--name", name, *mount,
                            "-e", "CUBE_EGRESS_DNS_RESOLVER_ADDRS=127.0.0.1:15353",
                            args.openresty_image, "openresty", "-c", "/fixture/nginx.conf",
                            "-g", "daemon off;"], check=True, stdout=subprocess.DEVNULL)
            command = ["docker", "run", "--rm", "--network", "container:" + name, *mount,
                       args.python_image, "python", "/repo/tests/dns_auth_integration.py", "--inside"]
            if args.baseline_ref:
                command += ["--baseline-ref", args.baseline_ref]
            subprocess.run(command, check=True, timeout=120)
            service_name = name + "-fixtures"
            try:
                subprocess.run(["docker", "run", "-d", "--name", service_name,
                                "--network", "container:" + name, *mount,
                                args.python_image, "python", "/repo/tests/dns_auth_integration.py",
                                "--inside", "--serve-only"], check=True, stdout=subprocess.DEVNULL)
                for _ in range(30):
                    ready = subprocess.run(["docker", "exec", service_name, "test", "-f", "/tmp/ready"],
                                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    if ready.returncode == 0:
                        break
                    time.sleep(0.1)
                else:
                    raise AssertionError("curl fixtures did not start")
                for scheme, port in [("http", 80), ("https", 443)]:
                    for method in ["hosts", "resolve"]:
                        curl = ["docker", "run", "--rm", "--network", "container:" + name,
                                "-v", str(fixture / "hosts") + ":/etc/hosts:ro", *mount,
                                args.curl_image, "--silent", "--show-error", "--max-time", "10",
                                "--noproxy", "*", "--cacert", "/fixture/cert.pem",
                                "-o", "/dev/null", "-w", "%{http_code}"]
                        if method == "resolve":
                            curl += ["--resolve", "allowed.example.test:%d:127.0.0.3" % port]
                        curl += [scheme + "://allowed.example.test/"]
                        status = subprocess.check_output(curl, text=True).strip()
                        expected = "200" if args.baseline_ref else "403"
                        assert status == expected, (scheme, method, status)
                        print("curl %s via container %s override: %s" % (scheme, method, status))
            finally:
                subprocess.run(["docker", "rm", "-f", service_name], check=False, stdout=subprocess.DEVNULL)
            audit_text = subprocess.check_output(["docker", "exec", name, "cat", "/tmp/audit.jsonl"], text=True)
            rows = [json.loads(line) for line in audit_text.splitlines()]
            if not args.baseline_ref:
                events = [r for r in rows if r.get("event") == "security_event"]
                assert any(r.get("dns_auth") for r in events), "missing DNS security audit"
                assert "fixture-secret" not in audit_text, "audit leaked injected value"
                print("audit: PASS (structured records, DNS denial details, secret redaction)")
            else:
                print("baseline audit: structured records parsed")
        except Exception:
            subprocess.run(["docker", "logs", "--tail", "60", name], check=False)
            raise
        finally:
            subprocess.run(["docker", "rm", "-f", name], check=False, stdout=subprocess.DEVNULL)


def wire_name(name):
    return b"".join(bytes([len(part)]) + part.encode("ascii") for part in name.split(".")) + b"\0"


class DNS(socketserver.BaseRequestHandler):
    records = {}
    delays = {}
    queries = []

    def handle(self):
        data, sock = self.request
        offset, labels = 12, []
        while data[offset]:
            size = data[offset]
            labels.append(data[offset + 1:offset + 1 + size].decode("ascii"))
            offset += size + 1
        name = ".".join(labels).lower()
        self.queries.append(name)
        time.sleep(self.delays.get(name, 0))
        question = data[12:offset + 5]
        records = self.records.get(name)
        if records == "timeout":
            return
        rcode = 3 if records is None else 0
        answers = []
        for owner, kind, value, ttl in records or []:
            payload = socket.inet_aton(value) if kind == 1 else wire_name(value)
            answers.append(wire_name(owner) + struct.pack("!HHIH", kind, 1, ttl, len(payload)) + payload)
        header = data[:2] + struct.pack("!HHHHH", 0x8180 | rcode, 1, len(answers), 0, 0)
        sock.sendto(header + question + b"".join(answers), self.client_address)


class Upstream(http.server.BaseHTTPRequestHandler):
    hits = []

    def do_GET(self):
        self.hits.append((self.server.server_address, dict(self.headers)))
        payload = json.dumps({"host": self.headers.get("Host"),
                              "injected": self.headers.get("X-Test-Secret")}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


def inside_run(args):
    dns = socketserver.ThreadingUDPServer(("127.0.0.1", 15353), DNS)
    threading.Thread(target=dns.serve_forever, daemon=True).start()
    cert = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    cert.load_cert_chain("/fixture/cert.pem", "/fixture/key.pem")
    for ip in ["127.0.0.2", "127.0.0.3"]:
        for port in [18080, 18443]:
            server = http.server.ThreadingHTTPServer((ip, port), Upstream)
            if port == 18443:
                server.socket = cert.wrap_socket(server.socket, server_side=True)
            threading.Thread(target=server.serve_forever, daemon=True).start()
    client_tls = ssl.create_default_context(cafile="/fixture/cert.pem")

    def policy(match, inject=False, allow=True):
        action = {"allow": allow, "audit": "metadata"}
        if inject:
            action["inject"] = [{"header": "X-Test-Secret", "secret": "fixture-secret",
                                 "secret_ref_synthetic": "test-fixture"}]
        body = json.dumps({"policy_id": "integration", "rules": [
            {"id": "tested", "match": match, "action": action},
            {"id": "fallback", "match": {}, "action": {"allow": True}}]})
        connection = http.client.HTTPConnection("127.0.0.1", 9091, timeout=5)
        connection.request("PUT", "/admin/v1/policies/fixture", body, {"Content-Type": "application/json"})
        assert connection.getresponse().status == 200
        connection.close()

    def request(label, ip="127.0.0.2", host="allowed.example.test", tls=False,
                sni=None, expected=200, injected=None, quiet=False):
        before = len(Upstream.hits)
        sock = socket.create_connection((ip, 443 if tls else 80), timeout=15)
        if tls:
            sock = client_tls.wrap_socket(sock, server_hostname=sni or host)
        sock.sendall(("GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n").encode())
        response = http.client.HTTPResponse(sock)
        response.begin()
        body = response.read()
        assert response.status == expected, (label, response.status, expected, body)
        if expected == 403:
            assert len(Upstream.hits) == before, label + ": denied request reached upstream"
        elif expected == 200:
            assert json.loads(body)["injected"] == injected, (label, body)
        sock.close()
        if not quiet:
            print(label + ": " + str(expected))

    for _ in range(50):
        try:
            policy({"host": "*.example.test"})
            break
        except OSError:
            time.sleep(0.1)
    else:
        raise AssertionError("nginx did not start")

    DNS.records["allowed.example.test"] = [("allowed.example.test", 1, "127.0.0.2", 30)]
    if args.serve_only:
        Path("/tmp/ready").touch()
        threading.Event().wait()
    bypass_status = 200 if args.baseline_ref else 403
    request("normal HTTP")
    request("forged HTTP destination", ip="127.0.0.3", expected=bypass_status)
    request("normal HTTPS", tls=True)
    request("forged HTTPS destination with valid certificate", tls=True, ip="127.0.0.3", expected=bypass_status)
    request("Host-only HTTPS policy with other SNI on shared IP", tls=True,
            sni="blocked.example.test", expected=bypass_status)
    if args.baseline_ref:
        print("BASELINE: HTTP and HTTPS bypass reproduced in socket fixture")
        return

    policy({"sni": "*.example.test", "scheme": "https"})
    request("SNI-only normal", tls=True)
    request("SNI-only forged destination", tls=True, ip="127.0.0.3", expected=403)
    policy({"host": "*.example.test"}, inject=True)
    request("HTTP credential injection", injected="fixture-secret")
    request("HTTPS credential injection", tls=True, injected="fixture-secret")
    request("deny before credential injection", ip="127.0.0.3", expected=403)
    policy({"host": "*.example.test"})
    DNS.records["alias.example.test"] = [("alias.example.test", 5, "target.example.test", 1),
                                         ("target.example.test", 1, "127.0.0.2", 30)]
    request("inline CNAME", host="alias.example.test")
    DNS.records["alias.example.test"] = [("alias.example.test", 1, "127.0.0.3", 30)]
    time.sleep(1.2)
    request("CNAME TTL expiry", host="alias.example.test", expected=403)
    DNS.records["zero.example.test"] = [("zero.example.test", 1, "127.0.0.2", 0)]
    request("TTL zero first answer", host="zero.example.test")
    DNS.records["zero.example.test"] = [("zero.example.test", 1, "127.0.0.3", 0)]
    request("TTL zero refreshed", host="zero.example.test", expected=403)
    DNS.records["poison.example.test"] = [("unrelated.example.test", 1, "127.0.0.2", 30)]
    request("unrelated DNS owner", host="poison.example.test", expected=403)
    request("NXDOMAIN", host="missing.example.test", expected=403)
    DNS.records["timeout.example.test"] = "timeout"
    request("DNS timeout", host="timeout.example.test", expected=403)

    DNS.records["parallel.example.test"] = [("parallel.example.test", 1, "127.0.0.2", 0)]
    DNS.delays["parallel.example.test"] = 0.5
    with ThreadPoolExecutor(max_workers=32) as pool:
        list(pool.map(lambda _: request("coalesced", host="parallel.example.test", quiet=True), range(32)))
    assert DNS.queries.count("parallel.example.test") == 1, DNS.queries
    print("32 overlapping TTL-zero requests: one DNS query")

    DNS.delays["parallel-failure.example.test"] = 0.5
    with ThreadPoolExecutor(max_workers=32) as pool:
        list(pool.map(lambda _: request("coalesced failure", host="parallel-failure.example.test",
                                       expected=403, quiet=True), range(32)))
    assert DNS.queries.count("parallel-failure.example.test") == 1
    print("32 overlapping NXDOMAIN requests: one DNS query, all denied")

    DNS.records["abort.example.test"] = [("abort.example.test", 1, "127.0.0.2", 0)]
    DNS.delays["abort.example.test"] = 0.5
    abandoned = socket.create_connection(("127.0.0.2", 80))
    abandoned.sendall(b"GET / HTTP/1.1\r\nHost: abort.example.test\r\n\r\n")
    time.sleep(0.1)
    abandoned.close()
    request("initiating client disconnected", host="abort.example.test")
    assert DNS.queries.count("abort.example.test") == 1

    names = ["cap" + str(i) + ".example.test" for i in range(40)]
    for name in names:
        DNS.records[name] = "timeout"
    with ThreadPoolExecutor(max_workers=40) as pool:
        list(pool.map(lambda name: request("bounded", host=name, expected=403, quiet=True), names))
    queried_names = set(DNS.queries).intersection(names)
    assert len(queried_names) <= 16, len(queried_names)
    print("40 distinct concurrent misses: at most 16 active names per worker, all denied")

    for i in range(7):
        name = "slow" + str(i) + ".example.test"
        DNS.records[name] = [(name, 5, "slow" + str(i + 1) + ".example.test", 30)]
        DNS.delays[name] = 1.2
    started = time.monotonic()
    request("total CNAME deadline", host="slow0.example.test", expected=403)
    elapsed = time.monotonic() - started
    assert 4.5 <= elapsed < 6.5, elapsed
    print("whole-chain timeout: %.2fs (5s budget)" % elapsed)
    DNS.records["recovery.example.test"] = [("recovery.example.test", 1, "127.0.0.2", 30)]
    request("DNS slots recovered after timeout", host="recovery.example.test")
    policy({"host": "127.0.0.2"})
    request("IP literal", host="127.0.0.2")
    request("forged IP literal", host="127.0.0.2", ip="127.0.0.3", expected=403)
    policy({"host": "*.example.test"}, allow=False)
    request("explicit deny takes precedence", expected=403)
    policy({})
    request("unconstrained rule retains behavior", ip="127.0.0.3")
    print("FIXED: all integration cases passed; every 403 left upstream hit count unchanged")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--serve-only", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--baseline-ref", help="reproduce using an older access_phase.lua git ref")
    parser.add_argument("--openresty-image", default=os.environ.get("OPENRESTY_TEST_IMAGE", "openresty/openresty:alpine"))
    parser.add_argument("--python-image", default=os.environ.get("PYTHON_TEST_IMAGE", "python:3.12-alpine"))
    parser.add_argument("--curl-image", default=os.environ.get("CURL_TEST_IMAGE", "curlimages/curl:8.16.0"))
    arguments = parser.parse_args()
    if arguments.inside:
        inside_run(arguments)
    else:
        host_run(arguments)
