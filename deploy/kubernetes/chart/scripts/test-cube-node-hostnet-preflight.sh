#!/usr/bin/env bash
# Unit tests for cube-node-hostnet-preflight.sh (decision logic) plus render
# checks for the Hook it ships in. No cluster required.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
PREFLIGHT="$CHART_DIR/files/cube-node-hostnet-preflight.sh"

CUBE_NODE_HOSTNET_PREFLIGHT_SOURCE_ONLY=1
# shellcheck disable=SC1090
. "$PREFLIGHT"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_decision() {
  expected="$1"
  live="$2"
  desired="$3"
  ack="$4"
  got="$(hostnet_change_decision "$live" "$desired" "$ack")"
  if [ "$got" != "$expected" ]; then
    fail "hostnet_change_decision '$live' '$desired' '$ack' = '$got', expected '$expected'"
  fi
}

# No DaemonSet yet: a fresh install has no netns to strand.
assert_decision keep absent true false
assert_decision keep absent false false

# A DaemonSet whose hostNetwork is omitted is on the Pod network (k8s default),
# so the empty jsonpath result must compare as false.
assert_decision keep "" false false
assert_decision keep "false" false false
assert_decision keep "true" true false
assert_decision keep "TRUE" true false

# Any change strands the netns, in both directions, until it is acknowledged.
assert_decision block "false" true false
assert_decision block "true" false false
assert_decision block "" true false
assert_decision ack-ok "false" true true
assert_decision ack-ok "true" false true
assert_decision ack-ok "" true TRUE

# The refusal has to name both ways out, or operators invent their own.
message="$(block_message false true cube-system cube-node)"
case "$message" in
  *"cubeNode.hostNetwork: false"*) ;;
  *) fail "block message must offer keeping the live value: $message" ;;
esac
case "$message" in
  *"cubeNode.hostNetworkChangeAck: true"*) ;;
  *) fail "block message must offer the acknowledgement: $message" ;;
esac
case "$message" in
  *"node-operations.md"*) ;;
  *) fail "block message must point at the drain procedure: $message" ;;
esac

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# main() against a fake kubectl: the refusal has to reach the operator as an exit
# code, and the "cannot read the DaemonSet" path must not read as "safe to flip".
FAKE_BIN="$TMP_DIR/bin"
mkdir -p "$FAKE_BIN"

write_fake_kubectl() {
  result="$1"
  ok="$2"
  cat > "$FAKE_BIN/kubectl" <<EOF
#!/bin/bash
if [ "${ok}" = "true" ]; then
  printf '%s' '${result}'
  exit 0
fi
printf '%s' '${result}' >&2
exit 1
EOF
  chmod +x "$FAKE_BIN/kubectl"
}

run_main() {
  desired="$1"
  ack="$2"
  env -i PATH="$FAKE_BIN:$PATH" \
    RELEASE_NAMESPACE=cube-system \
    CUBE_NODE_DS_NAME=cube-node \
    CUBE_NODE_HOSTNETWORK_DESIRED="$desired" \
    CUBE_NODE_HOSTNETWORK_CHANGE_ACK="$ack" \
    bash "$PREFLIGHT"
}

assert_main_fails() {
  what="$1"
  shift
  if output="$(run_main "$@" 2>&1)"; then
    fail "expected the preflight to refuse: ${what} (output: ${output})"
  fi
  case "$output" in
    *"cubeNode.hostNetworkChangeAck: true"*) ;;
    *) fail "refusal for ${what} must offer the acknowledgement: ${output}" ;;
  esac
}

assert_main_passes() {
  what="$1"
  shift
  if ! output="$(run_main "$@" 2>&1)"; then
    fail "expected the preflight to pass: ${what} (output: ${output})"
  fi
}

# A live DaemonSet on the old default: flipping it needs the acknowledgement.
write_fake_kubectl '' true
assert_main_fails "Pod network -> host network without ack" true false

# Acknowledged: the upgrade proceeds, but says what it is about to do.
assert_main_passes "acknowledged switch" true true
case "$(run_main true true 2>&1)" in
  *WARNING*) ;;
  *) fail "an acknowledged switch must still warn" ;;
esac

# Already in the desired mode, and a fresh install, both pass silently.
write_fake_kubectl 'true' true
assert_main_passes "unchanged host network" true false
write_fake_kubectl 'Error from server (NotFound): daemonsets.apps "cube-node" not found' false
assert_main_passes "fresh install" true false

# A stale acknowledgement left in values must not silently arm the next change.
write_fake_kubectl 'true' true
if output="$(run_main true true 2>&1)"; then
  case "$output" in
    *"remove it"*) ;;
    *) fail "keep with a stale ack must warn about removing it: ${output}" ;;
  esac
else
  fail "keep with a stale ack must still pass: ${output}"
fi

# The reverse switch (host network -> Pod network) is gated the same way.
assert_main_fails "host network -> Pod network without ack" false false
assert_main_passes "acknowledged reverse switch" false true
case "$(run_main false true 2>&1)" in
  *WARNING*) ;;
  *) fail "an acknowledged reverse switch must still warn" ;;
esac

# Any other API error must fail the release rather than pass as "no DaemonSet".
write_fake_kubectl 'Unable to connect to the server: dial tcp: i/o timeout' false
if output="$(run_main true false 2>&1)"; then
  fail "an unreadable DaemonSet must fail the release, not pass: ${output}"
fi
case "$output" in
  *"could not read DaemonSet"*) ;;
  *) fail "the API error must be reported as such: ${output}" ;;
esac

render() {
  output="$1"
  shift
  helm template hostnet-preflight "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

render "$TMP_DIR/base.yaml"
render "$TMP_DIR/optout.yaml" --set cubeNode.hostNetwork=false --set cubeNode.hostNetworkChangeAck=true
render "$TMP_DIR/no-node.yaml" --set cubeNode.enabled=false

# The Hook has to exist by default, with RBAC to read the DaemonSet and the
# values that decide the outcome wired into its env.
for kind in ServiceAccount Role RoleBinding ConfigMap Job; do
  if ! grep -q "^kind: ${kind}$" "$TMP_DIR/base.yaml" \
    || ! grep -q "name: hostnet-preflight-cube-cube-node-hostnet-preflight" "$TMP_DIR/base.yaml"; then
    fail "default render must ship the ${kind} for the hostnet preflight Hook"
  fi
done

if ! grep -q 'app.kubernetes.io/component: cube-node-hostnet-preflight' "$TMP_DIR/base.yaml"; then
  fail "Hook resources must carry the component label"
fi

python3 - "$TMP_DIR/base.yaml" <<'PY' || exit 1
import pathlib
import re
import sys

docs = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
job = [d for d in docs if "\nkind: Job\n" in f"\n{d}\n"
       and "cube-node-hostnet-preflight" in d]
if len(job) != 1:
    raise SystemExit(f"expected one hostnet preflight Job, found {len(job)}")
job = job[0]
for expected in (
    'helm.sh/hook-weight: "-109"',             # after the cubevs CIDR preflight (-110)
    'CUBE_NODE_HOSTNETWORK_DESIRED',
    'CUBE_NODE_HOSTNETWORK_CHANGE_ACK',
    'RELEASE_NAMESPACE',
    'CUBE_NODE_DS_NAME',
    'hostnet-preflight-cube-node',             # the DaemonSet it inspects
    'defaultMode: 0755',
    'restartPolicy: Never',
):
    if expected not in job:
        raise SystemExit(f"Hook Job is missing {expected!r}")
# Pin the events, and reject a value without pre-rollback: without it,
# helm rollback / --atomic can apply a hostNetwork:false manifest with no gate.
hook = re.search(r"helm\.sh/hook: ([^\n]+)", job)
if hook is None:
    raise SystemExit("Hook Job is missing helm.sh/hook")
if hook.group(1) == "pre-install,pre-upgrade":
    raise SystemExit("stale hook annotation without pre-rollback must not render")
if hook.group(1) != "pre-install,pre-upgrade,pre-rollback":
    raise SystemExit(f"Hook events must include pre-rollback, got {hook.group(1)!r}")
if 'name: CUBE_NODE_HOSTNETWORK_DESIRED\n              value: "true"' not in job:
    raise SystemExit("desired hostNetwork must render as true by default")
if 'name: CUBE_NODE_HOSTNETWORK_CHANGE_ACK\n              value: "false"' not in job:
    raise SystemExit("the acknowledgement must default to false")

# The Role must stay read-only on exactly the DaemonSet the Hook inspects.
role = [d for d in docs if "\nkind: Role\n" in f"\n{d}\n"
        and "cube-node-hostnet-preflight" in d]
if len(role) != 1:
    raise SystemExit(f"expected one hostnet preflight Role, found {len(role)}")
role = role[0]
if 'resources: ["daemonsets"]' not in role or 'verbs: ["get"]' not in role:
    raise SystemExit("the Role must be get-only on daemonsets")
PY

# The opt-out path must reach the Hook as data, and disabling cube-node must not
# ship a Hook that has no DaemonSet to guard.
if ! grep -q 'value: "false"' "$TMP_DIR/optout.yaml"; then
  fail "cubeNode.hostNetwork=false must reach the Hook"
fi
if ! grep -q 'CUBE_NODE_HOSTNETWORK_CHANGE_ACK' "$TMP_DIR/optout.yaml"; then
  fail "the acknowledgement must be passed through"
fi
python3 - "$TMP_DIR/optout.yaml" "$TMP_DIR/no-node.yaml" <<'PY' || exit 1
import pathlib
import sys

optout = pathlib.Path(sys.argv[1]).read_text()
no_node = pathlib.Path(sys.argv[2]).read_text()

job = [d for d in optout.split("\n---\n") if "\nkind: Job\n" in f"\n{d}\n"
       and "cube-node-hostnet-preflight" in d][0]
if 'name: CUBE_NODE_HOSTNETWORK_DESIRED\n              value: "false"' not in job:
    raise SystemExit("desired hostNetwork must be false in the opt-out render")
if 'name: CUBE_NODE_HOSTNETWORK_CHANGE_ACK\n              value: "true"' not in job:
    raise SystemExit("the acknowledgement must render as true")

if "cube-node-hostnet-preflight" in no_node:
    raise SystemExit("cubeNode.enabled=false must not render the Hook")
PY

# The script in the ConfigMap has to be the file on disk: a drifted copy is the
# one failure mode a render test can catch that a unit test cannot.
python3 - "$TMP_DIR/base.yaml" "$CHART_DIR/files/cube-node-hostnet-preflight.sh" <<'PY' || exit 1
import pathlib
import sys

docs = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
cm = [d for d in docs if "\nkind: ConfigMap\n" in f"\n{d}\n"
      and "cube-node-hostnet-preflight.sh" in d][0]
body = cm.split("cube-node-hostnet-preflight.sh: |\n", 1)[1]
body = "\n".join(line[4:] if line.startswith("    ") else line
                 for line in body.splitlines())
on_disk = pathlib.Path(sys.argv[2]).read_text()
if on_disk.strip() not in body.strip():
    raise SystemExit("ConfigMap script does not match files/cube-node-hostnet-preflight.sh")
PY

echo "cube-node-hostnet-preflight guard passed"
