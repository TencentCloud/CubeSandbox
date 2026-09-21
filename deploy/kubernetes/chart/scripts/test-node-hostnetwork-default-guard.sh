#!/usr/bin/env bash
# Guard for cube-node's default network mode and for the host-port preflight that
# only makes sense under it. No cluster required.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "$CHART_DIR/../../.." && pwd)"
NODE_PREP_LIB="$REPO_ROOT/deploy/kubernetes/images/scripts/node-prep-lib.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

render() {
  output="$1"
  shift
  helm template node-hostnet "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

extract_big_pod() {
  input="$1"
  output="$2"
  python3 - "$input" "$output" <<'PY'
import pathlib
import re
import sys

documents = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
matches = [
    doc for doc in documents
    if re.search(r"(?m)^kind:\s*DaemonSet\s*$", doc)
    and re.search(r"(?m)^\s*app\.kubernetes\.io/component: cube-node\s*$", doc)
]
if len(matches) != 1:
    raise SystemExit(f"expected one cube-node DaemonSet, found {len(matches)}")
pathlib.Path(sys.argv[2]).write_text(matches[0] + "\n")
PY
}

render "$TMP_DIR/base.yaml"
render "$TMP_DIR/s3lvol.yaml" --set cubeS3lvol.enabled=true
render "$TMP_DIR/podnet.yaml" \
  --set cubeNode.hostNetwork=false \
  --set bootstrap.nodeInit.checkHostPorts=false
extract_big_pod "$TMP_DIR/base.yaml" "$TMP_DIR/base-node.yaml"
extract_big_pod "$TMP_DIR/s3lvol.yaml" "$TMP_DIR/s3lvol-node.yaml"
extract_big_pod "$TMP_DIR/podnet.yaml" "$TMP_DIR/podnet-node.yaml"

# The default decides whether sandbox tap devices survive Big Pod recreation, so
# it is asserted rather than left to the values file alone.
grep -qx '      hostNetwork: true' "$TMP_DIR/base-node.yaml" \
  || fail "cube-node must default to the host network"
grep -qx '      dnsPolicy: ClusterFirstWithHostNet' "$TMP_DIR/base-node.yaml" \
  || fail "host network requires ClusterFirstWithHostNet for in-cluster DNS"

# Opting into the Pod network flips both, so the opt-out stays usable.
grep -qx '      hostNetwork: false' "$TMP_DIR/podnet-node.yaml" \
  || fail "cubeNode.hostNetwork=false must render the Pod network"
grep -qx '      dnsPolicy: ClusterFirst' "$TMP_DIR/podnet-node.yaml" \
  || fail "the Pod network must render ClusterFirst"

# hostNetwork is a Pod field: cube-s3lvol inherits it and must not carry its own
# copy, which the API server would reject.
if [ "$(grep -c 'hostNetwork:' "$TMP_DIR/s3lvol-node.yaml")" != "1" ]; then
  fail "hostNetwork must be declared once per Pod"
fi
grep -q 'name: cube-s3lvol' "$TMP_DIR/s3lvol-node.yaml" \
  || fail "expected the s3lvol sidecar in the same Pod"
grep -qx '      hostNetwork: true' "$TMP_DIR/s3lvol-node.yaml" \
  || fail "the sidecar must inherit the Pod's network mode"

# The host-port preflight runs from cube-node-init, so the Bootstrap DaemonSet
# has to carry both the switch and the mode that gates it.
extract_bootstrap() {
  python3 - "$1" <<'PY'
import pathlib
import sys

documents = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
matches = [doc for doc in documents
           if "app.kubernetes.io/component: cube-node-bootstrap" in doc
           and "\nkind: DaemonSet\n" in f"\n{doc}\n"]
if len(matches) != 1:
    raise SystemExit(f"expected one cube-node-bootstrap DaemonSet, found {len(matches)}")
print(matches[0])
PY
}
extract_bootstrap "$TMP_DIR/base.yaml" > "$TMP_DIR/base-bootstrap.yaml"
extract_bootstrap "$TMP_DIR/podnet.yaml" > "$TMP_DIR/podnet-bootstrap.yaml"

grep -q 'name: CHECK_HOST_PORTS' "$TMP_DIR/base-bootstrap.yaml" \
  || fail "bootstrap must pass CHECK_HOST_PORTS to cube-node-init"
grep -q 'name: CUBE_NODE_HOST_NETWORK' "$TMP_DIR/base-bootstrap.yaml" \
  || fail "bootstrap must pass the network mode to cube-node-init"
python3 - "$TMP_DIR/base-bootstrap.yaml" "$TMP_DIR/podnet-bootstrap.yaml" <<'PY' || exit 1
import pathlib
import re
import sys

def envs(path):
    text = pathlib.Path(path).read_text()
    return dict(re.findall(r"- name: (\w+)\n\s+value: \"?([^\n\"]*)\"?", text))

base, podnet = envs(sys.argv[1]), envs(sys.argv[2])
if base.get("CHECK_HOST_PORTS") != "true":
    raise SystemExit(f"CHECK_HOST_PORTS must default to true, got {base.get('CHECK_HOST_PORTS')!r}")
if base.get("CUBE_NODE_HOST_NETWORK") != "true":
    raise SystemExit("the default render must tell node-init it is on the host network")
if base.get("HOST_PORT_RESERVED_PORTS") != "9998 9999 9966":
    raise SystemExit(f"unexpected default port set: {base.get('HOST_PORT_RESERVED_PORTS')!r}")
if base.get("PREP_GENERATION") != "2":
    raise SystemExit(f"prepGeneration must be pinned at 2 so already-ready nodes re-run node-init, got {base.get('PREP_GENERATION')!r}")
if podnet.get("CUBE_NODE_HOST_NETWORK") != "false":
    raise SystemExit("the opt-out render must tell node-init it is on the Pod network")
if podnet.get("CHECK_HOST_PORTS") != "false":
    raise SystemExit("bootstrap.nodeInit.checkHostPorts=false must reach node-init")
PY

render "$TMP_DIR/s3lvol-pods.yaml" --set cubeS3lvol.enabled=true --set-string cubeS3lvol.listenPort=5520
extract_bootstrap "$TMP_DIR/s3lvol-pods.yaml" > "$TMP_DIR/s3lvol-bootstrap.yaml"
grep -q 'HOST_PORT_RESERVED_PORTS' "$TMP_DIR/s3lvol-bootstrap.yaml" || fail "expected the port env"
if ! grep -A1 'name: HOST_PORT_RESERVED_PORTS' "$TMP_DIR/s3lvol-bootstrap.yaml" | grep -q '"9998 9999 9966 5520"'; then
  fail "enabling s3lvol must add its listen port to the checked set: $(grep -A1 HOST_PORT_RESERVED_PORTS "$TMP_DIR/s3lvol-bootstrap.yaml")"
fi

# The sentinel writer (write-node-prep-ready) must see the same reserved-port
# set as cube-node-init, or its host_ports fingerprint field can never match
# and SKIP_IF_NODE_PREP_READY is permanently defeated.
python3 - "$TMP_DIR/base-bootstrap.yaml" "$TMP_DIR/s3lvol-bootstrap.yaml" <<'PY' || exit 1
import pathlib
import re
import sys

def env_block(text, container):
    m = re.search(r"- name: " + re.escape(container) + r"\n.*?(?=\n      - name: |\n      volumes:|\Z)", text, re.S)
    return m.group(0) if m else ""

def env_value(block, key):
    m = re.search(r"- name: " + re.escape(key) + r"\n\s+value: \"?([^\n\"]*)\"?", block)
    return m.group(1) if m else None

for path in (sys.argv[1], sys.argv[2]):
    text = pathlib.Path(path).read_text()
    init = env_block(text, "cube-node-init")
    writer = env_block(text, "write-node-prep-ready")
    if not writer:
        raise SystemExit(f"write-node-prep-ready container not found in {path}")
    if "HOST_PORT_RESERVED_PORTS" not in writer:
        raise SystemExit(f"write-node-prep-ready must receive HOST_PORT_RESERVED_PORTS in {path}")
    if env_value(init, "HOST_PORT_RESERVED_PORTS") != env_value(writer, "HOST_PORT_RESERVED_PORTS"):
        raise SystemExit(f"init vs write-node-prep-ready HOST_PORT_RESERVED_PORTS mismatch in {path}")
PY

# Port resolution is tested against a fixture tree: the real one needs a host.
# /proc/net/tcp is hex, column 4 is the state (0A = LISTEN), column 10 the inode.
[ -f "$NODE_PREP_LIB" ] || fail "missing $NODE_PREP_LIB"
# 4420 stands in for the s3lvol listen port the chart only adds when it is on.
HOST_PORT_RESERVED_PORTS="9998 9999 9966 4420"
export HOST_PORT_RESERVED_PORTS
# shellcheck disable=SC1090
. "$NODE_PREP_LIB"

fp="$(node_prep_compute_fingerprint)"
case "$fp" in
  *"host_ports=${HOST_PORT_RESERVED_PORTS}"*) ;;
  *) fail "fingerprint must include the reserved-port set: $fp" ;;
esac
HOST_PORT_RESERVED_PORTS="9998 9999 9966"
fp_other="$(node_prep_compute_fingerprint)"
if [ "$fp" = "$fp_other" ]; then
  fail "changing the reserved-port set must change the fingerprint"
fi
HOST_PORT_RESERVED_PORTS="9998 9999 9966 4420"

fixture="$TMP_DIR/host/proc"
mkdir -p "$fixture/1/net" "$fixture/4242/fd" "$fixture/5555/fd" "$fixture/6666/fd" "$fixture/7777/fd"
cat > "$fixture/1/net/tcp" <<'EOF'
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:270F 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11111 1 0000000000000000 100 0 0 10 0
   1: 00000000:26EE 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11114 1 0000000000000000 100 0 0 10 0
   2: 0100007F:3039 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11113 1 0000000000000000 100 0 0 10 0
   3: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11112 1 0000000000000000 100 0 0 10 0
   4: 0100007F:1144 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 11115 1 0000000000000000 100 0 0 10 0
   5: 0100007F:1144 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11116 1 0000000000000000 100 0 0 10 0
EOF
cat > "$fixture/1/net/tcp6" <<'EOF'
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:270E 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11117 1 0000000000000000 100 0 0 10 0
EOF

# 9999 is held by our own cubelet (comm only — fallback), 9966 by a foreign
# nginx, 4420 by a thread whose comm is not in the allow-list but whose exe
# is, 8080 and 12345 are none of our business. 9998 (tcp6) has no holder.
printf 'cubelet\n' > "$fixture/4242/comm"
ln -s 'socket:[11111]' "$fixture/4242/fd/7"
printf 's3lvol_tgt\n' > "$fixture/5555/comm"
ln -s 'socket:[11112]' "$fixture/5555/fd/3"
printf 'nginx\n' > "$fixture/6666/comm"
ln -s 'socket:[11114]' "$fixture/6666/fd/9"
printf 'reactor_0\n' > "$fixture/7777/comm"
ln -s '/opt/s3lvol/bin/s3lvol_tgt (deleted)' "$fixture/7777/exe"
ln -s 'socket:[11116]' "$fixture/7777/fd/4"

listened="$(host_listen_ports "$fixture/1/net")"
case "$listened" in
  *"9999 11111"*) ;;
  *) fail "host_listen_ports must report LISTEN sockets: $listened" ;;
esac
case "$listened" in
  *"9998 11117"*) ;;
  *) fail "host_listen_ports must read tcp6 too: $listened" ;;
esac
case "$listened" in
  *"9999 11115"*) fail "state 01 is not LISTEN: $listened" ;;
  *) ;;
esac
case "$listened" in
  *"8080 11112"*) ;;
  *) fail "host_listen_ports reports every port; filtering is the caller's: $listened" ;;
esac
case "$listened" in
  *"12345 "*) ;;
  *) fail "host_listen_ports must not drop unrelated ports: $listened" ;;
esac

conflicts="$(host_port_conflicts "$fixture")"
case "$conflicts" in
  *"9999 held"*) fail "our own components must not count as a conflict: $conflicts" ;;
  *) ;;
esac
case "$conflicts" in
  *"port 9966 held by 6666 nginx"*) ;;
  *) fail "a foreign holder must be reported with pid and comm: $conflicts" ;;
esac
case "$conflicts" in
  *"4420 held"*) fail "exe basename must identify our sidecar even when comm is a reactor thread: $conflicts" ;;
  *) ;;
esac
case "$conflicts" in
  *"port 9998 held by unknown"*) ;;
  *) fail "a holder with no reachable process must be reported as unknown: $conflicts" ;;
esac
case "$conflicts" in
  *"8080"*|*"12345"*) fail "unreserved ports are none of our business: $conflicts" ;;
  *) ;;
esac

# check_host_ports gating, extracted from cube-node-init.sh (sourcing the script
# would run every other check). The port preflight answers to its own switch and
# the network mode only — the CIDR skip must not turn it off.
init_body="$(sed -n '/^check_host_ports()/,/^}/p' "$REPO_ROOT/deploy/kubernetes/images/scripts/cube-node-init.sh")"
[ -n "$init_body" ] || fail "could not extract check_host_ports() from cube-node-init.sh"
eval "$init_body"

log() { :; }
host_path() { printf '%s%s' "${HOST_ROOT:-/host}" "$1"; }
host_port_conflicts() {
  [ "$1" = "/host/proc" ] || fail "check_host_ports must probe the host netns, got '$1'"
  printf -- '- port 9998 held by 0 stub\n'
}

run_check() {
  (
    CHECK_HOST_PORTS="$1"
    CUBE_NODE_HOST_NETWORK="$2"
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK="$3"
    check_host_ports
  )
}

assert_check_runs() {
  if output="$(run_check "$@" 2>&1)"; then
    fail "port check must run and report (CHECK_HOST_PORTS=$1 hostNetwork=$2 cidrSkip=$3): no conflict reported"
  fi
  case "$output" in
    *"host port conflict"*) ;;
    *) fail "port check must report the conflict (CHECK_HOST_PORTS=$1 hostNetwork=$2 cidrSkip=$3): $output" ;;
  esac
}

assert_check_skipped() {
  if ! output="$(run_check "$@" 2>&1)"; then
    fail "port check must be gated off (CHECK_HOST_PORTS=$1 hostNetwork=$2 cidrSkip=$3): $output"
  fi
}

assert_check_runs true true 0
assert_check_runs true true 1
assert_check_skipped false true 0
assert_check_skipped true false 0

# CIDR listing must enter the host netns when cube-node is on the host
# network; bootstrap itself is always on the Pod network.
cidr_fn="$(sed -n '/^cidr_ip()/,/^}/p' "$REPO_ROOT/deploy/kubernetes/images/scripts/cube-node-init.sh")"
[ -n "$cidr_fn" ] || fail "could not extract cidr_ip() from cube-node-init.sh"
eval "$cidr_fn"
ip() { printf 'ip %s\n' "$*"; }
nsenter() { printf 'nsenter %s\n' "$*"; }
host_listed="$(CUBE_NODE_HOST_NETWORK=true cidr_ip -o -4 addr show)"
case "$host_listed" in
  *"nsenter --target 1 --net -- ip -o -4 addr show"*) ;;
  *) fail "hostNetwork CIDR check must nsenter the host netns: $host_listed" ;;
esac
pod_listed="$(CUBE_NODE_HOST_NETWORK=false cidr_ip -o -4 addr show)"
case "$pod_listed" in
  *"nsenter"*) fail "Pod-network CIDR check must stay in this netns: $pod_listed" ;;
  *"ip -o -4 addr show"*) ;;
  *) fail "Pod-network CIDR check must call ip: $pod_listed" ;;
esac
sed -n '/^check_cidr_conflict()/,/^}/p' "$REPO_ROOT/deploy/kubernetes/images/scripts/cube-node-init.sh" \
  | grep -q 'cidr_ip -o -4' \
  || fail "check_cidr_conflict must list addrs/routes via cidr_ip"

echo "cube-node host-network default guard passed"
