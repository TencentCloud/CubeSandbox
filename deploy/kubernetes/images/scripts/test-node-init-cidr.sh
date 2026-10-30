#!/bin/sh
# Real node-init/readiness scripts against a read-only mocked network inventory.
# No host network, namespace, filesystem mount, Kubernetes or KVM access required.
set -eu
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

python3 - "$SCRIPT_DIR" "$TMP_DIR" <<'PY'
import json
import os
import pathlib
import re
import subprocess
import sys

scripts, tmp = map(pathlib.Path, sys.argv[1:])
repo = scripts.parents[3]
mac = "20:90:6f:cf:cf:cf"
# A packaged gateway identity change must update this conservative preflight.
assert f'mvm_gw_mac_addr = "{mac}"' in (repo / "Cubelet/config/config.toml").read_text()
assert re.search(r'MvmGwMacAddr:\s*"' + mac + '"',
                 (repo / "Cubelet/network/runtime/config.go").read_text())

bin_dir = tmp / "bin"
bin_dir.mkdir()
probe = bin_dir / "probe"
probe.write_text("#!/usr/bin/env python3\n" + r'''
import json
import os
import pathlib
import subprocess
import sys
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ["PROBE_LOG"], "a") as log:
    log.write(json.dumps([name, *args]) + "\n")
if name == "nsenter":
    assert args[:6] == ["--target", "1", "--net", "--", "ip", "-o"] or args[:6] == ["--target", "1", "--net", "--", "ip", "-d"], args
    sys.exit(subprocess.run(args[4:]).returncode)
if name == "ip":
    inventory = json.loads(pathlib.Path(os.environ["INVENTORY"]).read_text())
    commands = {
        ("-o", "-4", "addr", "show"): "addrs",
        ("-o", "-4", "route", "show", "table", "all"): "routes",
        ("-d", "-o", "link", "show", "dev", "cube-dev", "type", "dummy"): "link",
    }
    assert tuple(args) in commands, ("unexpected network operation", args)
    key = commands[tuple(args)]
    if key == "link" and inventory.get("kind", "dummy") != "dummy":
        sys.exit(0)  # ip's link-kind filter must not return a same-name bridge.
    print(inventory.get(key, ""), end="")
    print(inventory.get(key + "_stderr", ""), end="", file=sys.stderr)
    sys.exit(inventory.get(key + "_exit", 0))
if name in ("lsmod", "xfs_info"):
    sys.exit(0)
if name == "sleep" and args == ["infinity"]:
    sys.exit(0)
raise AssertionError(("unexpected host operation", name, args))
''')
probe.chmod(0o755)
for name in ("ip", "nsenter", "lsmod", "xfs_info", "sleep", "mount", "umount",
             "modprobe", "chroot", "mkfs.xfs", "losetup", "truncate", "kubectl"):
    (bin_dir / name).symlink_to(probe)

eth = "2: eth0    inet 10.0.2.15/24 brd 10.0.2.255 scope global eth0\n"
addr = "8: cube-dev    inet 172.16.0.1/18 brd 172.16.63.255 scope global cube-dev\n"
link = (f"8: cube-dev: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN "
        f"mode DEFAULT group default qlen 1000\\    link/ether {mac} brd ff:ff:ff:ff:ff:ff\\    dummy\n")
route = "172.16.0.0/18 dev cube-dev proto kernel scope link src 172.16.0.1\n"
static = "172.16.0.0/18 dev cube-dev proto static scope link\n"
locals_ = ("broadcast 172.16.0.0 dev cube-dev table local proto kernel scope link src 172.16.0.1\n"
           "local 172.16.0.1 dev cube-dev table local proto kernel scope host src 172.16.0.1\n"
           "broadcast 172.16.63.255 dev cube-dev table local proto kernel scope link src 172.16.0.1\n")
base_routes = "default via 10.0.2.2 dev eth0\n10.0.2.0/24 dev eth0 proto kernel scope link src 10.0.2.15\n"
fresh = {"addrs": eth, "routes": base_routes}
owned = {"addrs": eth + addr, "routes": base_routes + route + locals_, "link": link}


def environment(name, inventory, **overrides):
    case = tmp / name
    case.mkdir()
    fixture = case / "network.json"
    fixture.write_text(json.dumps(inventory))
    env = {
        "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "LANG": "C",
        "HOST_ROOT": str(case / "host"), "STATE_DIR": "/bootstrap",
        "INVENTORY": str(fixture), "PROBE_LOG": str(case / "probes.jsonl"),
        "CHECK_CIDR": "true", "CUBE_SANDBOX_NETWORK_CIDR": "172.16.0.0/18",
        "CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK": "0",
        "CUBE_NODE_HOST_NETWORK": "true", "SKIP_IF_NODE_PREP_READY": "false",
        "PVM_ENABLED": "0", "CUBE_PVM_ENABLE": "0", "NODE_INIT_ENABLED": "1",
        "KERNEL_BOOT_ARGS": "", "DESIRED_KERNEL_PATTERN": "",
        "LOOPBACK_ENABLED": "false", "WAIT_TIMEOUT_SECONDS": "0",
        "WAIT_READY_MARKER": str(case / "wait-ready"),
    }
    for name in ("REQUIRE_KVM", "REQUIRE_XFS", "CHMOD_KVM", "LOAD_KVM_MODULE",
                 "WRITE_UDEV_RULE", "CREATE_HOST_DIRS", "CHECK_MASTER_CONNECTIVITY",
                 "CHECK_MEMORY", "CHECK_CGROUP_CPU", "CHECK_BPF_FS", "CHECK_GLIBC",
                 "CHECK_HOST_PORTS", "CHECK_CUBECOW_DEPS"):
        env[name] = "false"
    env.update(overrides)
    return env


def run(env, success=True, command=None):
    result = subprocess.run(command or ["sh", str(scripts / "cube-node-init.sh")],
                            env=env, capture_output=True, text=True, timeout=10)
    output = result.stdout + result.stderr
    assert (result.returncode == 0) == success, (env["INVENTORY"], result.returncode, output)
    return output


def case(name, inventory, success=True, reason=None, **overrides):
    env = environment(name, inventory, **overrides)
    output = run(env, success)
    if reason:
        assert reason in output, (name, output)
    if success:
        assert "cube node init completed" in output, output
    print("ok:", name)
    return env


case("fresh-host", fresh)
case("owned-gateway", owned, reason="reusing verified cube-dev gateway")
case("owned-local-host-prefixes", dict(owned, routes=base_routes + route + re.sub(r"((?:local|broadcast) [0-9.]+) ", r"\1/32 ", locals_)))
case("owned-static-route", dict(owned, routes=base_routes + static + locals_))
case("owned-uppercase-mac", dict(owned, link=link.replace(mac, mac.upper())))
case("owned-linkdown", dict(owned, routes=base_routes + route.rstrip() + " linkdown\n" + locals_))
case("foreign-bridge", dict(fresh, addrs=eth + addr.replace("cube-dev", "br-other")), False,
     "addr 172.16.0.1/18 br-other")
case("same-name-real-bridge", dict(owned, kind="bridge"), False, "addr 172.16.0.1/18 cube-dev")
case("same-name-wrong-mac", dict(owned, link=link.replace(mac, "02:00:00:00:00:01")), False)
case("same-name-missing-mac", dict(owned, link=link.replace("link/ether", "link/none")), False)
case("missing-link-metadata", dict(owned, link=""), False)
case("partial-link-dump", dict(owned, link_stderr="Dump terminated\n"), False)
case("failed-link-probe", dict(owned, link_exit=1), False)
case("wrong-link-name", dict(owned, link=link.replace("cube-dev:", "other:")), False)
case("wrong-gateway", dict(owned, addrs=eth + addr.replace("172.16.0.1/18", "172.16.0.2/18")), False)
case("wrong-prefix", dict(owned, addrs=eth + addr.replace("/18", "/19")), False)
case("overlapping-cidr-change", owned, False, CUBE_SANDBOX_NETWORK_CIDR="172.16.0.0/19")
case("extra-gateway-address", dict(owned, addrs=owned["addrs"] + addr.replace("172.16.0.1/18", "192.0.2.1/24")), False)
case("route-without-gateway-address", dict(owned, addrs=eth), False)
case("owned-plus-foreign-address", dict(owned, addrs=owned["addrs"] + addr.replace("cube-dev", "br-other")), False,
     "addr 172.16.0.1/18 br-other")
for name, foreign in {
    "foreign-route": "172.16.0.0/18 dev eth1 proto kernel scope link src 172.16.0.1",
    "foreign-more-specific-route": "172.16.1.0/24 dev eth1 proto static scope link",
    "foreign-supernet-route": "172.16.0.0/12 dev eth1 proto static scope link",
    "foreign-policy-route": "172.16.1.0/24 dev eth1 table 100 proto static scope link",
    "foreign-blackhole": "blackhole 172.16.1.0/24 table 100 proto static",
    "cube-dev-more-specific": "172.16.1.0/24 dev cube-dev proto static scope link",
    "cube-dev-multipath": "172.16.0.0/18 proto static nexthop dev cube-dev weight 1 nexthop dev eth1 weight 1",
    "cube-dev-via": "172.16.0.0/18 via 172.16.0.2 dev cube-dev proto static scope link",
    "cube-dev-policy-route": "172.16.0.0/18 dev cube-dev table 100 proto static scope link",
    "cube-dev-unknown-proto": "172.16.0.0/18 dev cube-dev proto boot scope link",
    "cube-dev-wrong-source": "172.16.0.0/18 dev cube-dev proto kernel scope link src 172.16.0.2",
    "cube-dev-missing-source": "172.16.0.0/18 dev cube-dev proto kernel scope link",
    "cube-dev-duplicate-device": "172.16.0.0/18 dev eth1 dev cube-dev proto static scope link",
    "cube-dev-missing-field-value": "172.16.0.0/18 dev cube-dev proto static scope link src",
    "cube-dev-static-with-foreign-source": "172.16.0.0/18 dev cube-dev proto static scope link src 172.16.0.2",
    "cube-dev-unexpected-metric": "172.16.0.0/18 dev cube-dev proto static scope link metric 9",
    "cube-dev-foreign-local": "local 172.16.0.2 dev cube-dev table local proto kernel scope host src 172.16.0.1",
    "cube-dev-foreign-broadcast": "broadcast 172.16.0.9 dev cube-dev table local proto kernel scope link src 172.16.0.1",
    "cube-dev-local-wrong-table": "local 172.16.0.1 dev cube-dev table 100 proto kernel scope host src 172.16.0.1",
}.items():
    case(name, dict(owned, routes=owned["routes"] + foreign + "\n"), False, foreign)
case("address-inventory-fails-closed", dict(fresh, addrs_exit=1), False, "unable to list IPv4 addresses")
case("route-inventory-fails-closed", dict(fresh, routes_exit=1), False, "unable to list IPv4 routes")
case("partial-address-dump-fails-closed", dict(fresh, addrs_stderr="Dump terminated\n"), False,
     "unable to list IPv4 addresses")
case("partial-route-dump-fails-closed", dict(fresh, routes_stderr="Dump was interrupted\n"), False,
     "unable to list IPv4 routes")
case("fresh-pod-netns", fresh, CUBE_NODE_HOST_NETWORK="false")
case("no-host-ownership-exemption-in-pod-netns", owned, False, CUBE_NODE_HOST_NETWORK="false")
case("explicit-skip", dict(owned, addrs_exit=1), CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK="1")
case("skip-still-validates-cidr", fresh, False, "not aligned", CUBE_SANDBOX_NETWORK_CIDR="172.16.0.1/18",
     CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK="1")

# Real invalidate -> node-init -> writer -> waiter transitions with the same
# retained gateway. No manual network cleanup or CHECK_CIDR=false bypass.
def helper(env, name):
    return run(env, command=["sh", "-ec", '. "$1"; ' + name, "cidr-test", str(scripts / "node-prep-lib.sh")])


def ready_scripts(env):
    run(env, command=["sh", str(scripts / "write-node-prep-ready.sh")])
    run(env, command=["sh", str(scripts / "wait-node-prep.sh")])


env = environment("storage-reprep", owned, SKIP_IF_NODE_PREP_READY="true", CREATE_HOST_DIRS="true")
run(env)
ready_scripts(env)
ready = pathlib.Path(env["HOST_ROOT"]) / "bootstrap/node-prep-ready"
for key in ("DATA_SHARED", "DATA_LOG", "TMP_CUBE"):
    env[key] = "/moved/" + key.lower()
    helper(env, "invalidate_stale_node_prep_ready")
    assert not ready.exists(), key
    assert "reusing verified cube-dev gateway" in run(env)
    assert (pathlib.Path(env["HOST_ROOT"]) / env[key].lstrip("/")).is_dir(), key
    ready_scripts(env)
    assert "skipping node-init" in run(env)
    print("ok: retained gateway permits path re-preparation", key)
ready.write_text("\n".join("version=1" if line == "version=2" else line
                          for line in ready.read_text().splitlines() if not line.startswith("storage_")) + "\n")
run(env, False, ["sh", str(scripts / "wait-node-prep.sh")])
helper(env, "invalidate_stale_node_prep_ready")
assert not ready.exists()
assert "reusing verified cube-dev gateway" in run(env)
ready_scripts(env)
assert ready.read_text().startswith("version=2\n")
print("ok: version-1 readiness upgrades without removing the existing gateway")

# Mocks accept only explicit read-only commands, and host vs Pod-netns dispatch
# must remain correct across all address, route and link metadata probes.
for path in tmp.glob("*/probes.jsonl"):
    calls = [json.loads(line) for line in path.read_text().splitlines()]
    if path.parent.name in ("fresh-pod-netns", "no-host-ownership-exemption-in-pod-netns"):
        assert not any(call[0] == "nsenter" for call in calls), calls
    elif any(call[0] == "ip" for call in calls):
        assert sum(call[0] == "nsenter" for call in calls) == sum(call[0] == "ip" for call in calls), calls
print("node-init CIDR idempotency regressions passed (mocked network; not live Kubernetes/XFS)")
PY
