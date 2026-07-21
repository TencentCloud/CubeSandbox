#!/bin/sh
set -eu

REPO_ROOT="$(CDPATH= cd -- "$(dirname "$0")/../../../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
mkdir -p "$TMP_DIR/bin" "$TMP_DIR/state"

cat > "$TMP_DIR/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$TEST_LOG"
case "$*" in
  "get node "*)
    if [ -f "$TEST_STATE/tainted" ]; then
      value="true"
      [ -f "$TEST_STATE/taint-value" ] && value="$(tr -d '[:space:]' < "$TEST_STATE/taint-value")"
      printf 'NoSchedule=%s ' "$value"
    fi
    ;;
  "taint node "*)
    [ "${FAIL_TAINT:-0}" = "1" ] && exit 1
    case "$*" in *-)
      [ "${FAIL_CLEAR:-0}" = "1" ] && exit 1
      rm -f "$TEST_STATE/tainted"
      rm -f "$TEST_STATE/taint-value"
      ;; *)
      : > "$TEST_STATE/tainted"
      case "$*" in
        *"=maintenance:"*) printf 'maintenance ' > "$TEST_STATE/taint-value" ;;
        *) printf 'true ' > "$TEST_STATE/taint-value" ;;
      esac
      ;;
    esac
    ;;
  *"get pods "*"--field-selector"*)
    printf 'cube-node-pvm\tcube-node-pvm\n'
    [ -f "$TEST_STATE/evicted-master" ] || printf 'cube-master\tmaster\n'
    ;;
  "create --raw "*)
    cat >/dev/null
    [ "${FAIL_EVICT:-0}" = "1" ] && exit 1
    : > "$TEST_STATE/evicted-master"
    ;;
esac
EOF
chmod +x "$TMP_DIR/bin/kubectl"

export PATH="$TMP_DIR/bin:$PATH"
export TEST_LOG="$TMP_DIR/kubectl.log"
export TEST_STATE="$TMP_DIR/state"
export NODE_NAME=test-node
export NAMESPACE=test-ns
export POD_NAMESPACE=test-ns
export CUBE_RELEASE=test-release
export STARTUP_GATE_ENABLED=true
export STARTUP_GATE_TAINT_KEY=cube.tencent.com/pvm-not-ready
export STARTUP_GATE_TAINT_EFFECT=NoSchedule
export STARTUP_GATE_DRAIN_TIMEOUT_SECONDS=2
export STATE_DIR="$TMP_DIR/sentinel"
export HOST_ROOT=
export PVM_ENABLED=1
export DESIRED_KERNEL_PATTERN="$(uname -r)"
export KERNEL_BOOT_ARGS=

log() { :; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# shellcheck disable=SC1090
. "$REPO_ROOT/deploy/kubernetes/images/scripts/node-prep-lib.sh"
# shellcheck disable=SC1090
. "$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-startup-gate-lib.sh"

ensure_startup_gate_taint
startup_gate_has_taint
if (clear_startup_gate_taint); then
  echo "clear unexpectedly accepted a missing fingerprint" >&2
  exit 1
fi
drain_startup_gate_dependents
grep -q 'create --raw /api/v1/namespaces/test-ns/pods/cube-master/eviction' "$TEST_LOG"
grep -q -- '--field-selector spec.nodeName=test-node' "$TEST_LOG"
grep -q 'app.kubernetes.io/instance=test-release' "$TEST_LOG"
# Drain must list name+component in one call (no per-pod get).
if grep -q 'get pod/cube-' "$TEST_LOG"; then
  echo "drain still performs per-pod component lookups" >&2
  exit 1
fi

write_pvm_host_ready
mark_pvm_mutating
if (clear_startup_gate_taint); then
  echo "clear unexpectedly accepted pvm-mutating state" >&2
  exit 1
fi
clear_pvm_mutating
if (
  export FAIL_CLEAR=1
  clear_startup_gate_taint
); then
  echo "clear API failure unexpectedly succeeded" >&2
  exit 1
fi
startup_gate_has_taint
clear_startup_gate_taint
if startup_gate_has_taint; then
  echo "taint was not cleared" >&2
  exit 1
fi

rm -f "$TEST_STATE/tainted" "$TEST_STATE/evicted-master"
: > "$TEST_LOG"
if (
  export FAIL_TAINT=1
  ensure_startup_gate_taint
  drain_startup_gate_dependents
); then
  echo "ensure failure unexpectedly succeeded" >&2
  exit 1
fi
if grep -q 'create --raw' "$TEST_LOG"; then
  echo "drain ran after ensure failure" >&2
  exit 1
fi

: > "$TEST_STATE/tainted"
rm -f "$TEST_STATE/evicted-master"
if (
  export FAIL_EVICT=1
  drain_startup_gate_dependents
); then
  echo "PDB/eviction rejection unexpectedly succeeded" >&2
  exit 1
fi
[ ! -f "$TEST_STATE/evicted-master" ]

rm -f "$TEST_STATE/tainted" "$(pvm_host_ready_path)"
STARTUP_GATE_RECONCILE_ONCE=true \
  sh "$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-startup-gate-reconcile.sh"
[ -f "$TEST_STATE/tainted" ]
write_pvm_host_ready
printf 'maintenance ' > "$TEST_STATE/taint-value"
STARTUP_GATE_RECONCILE_ONCE=true \
  sh "$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-startup-gate-reconcile.sh"
[ -f "$TEST_STATE/tainted" ]
STARTUP_GATE_CLEAR_MAINTENANCE=true clear_startup_gate_taint
[ ! -f "$TEST_STATE/tainted" ]

ensure_startup_gate_taint
STARTUP_GATE_RECONCILE_ONCE=true \
  sh "$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-startup-gate-reconcile.sh"
[ ! -f "$TEST_STATE/tainted" ]

# --- preflight Job script (cluster-level checks) ---
PREFLIGHT_SCRIPT="$REPO_ROOT/deploy/kubernetes/images/scripts/pvm-startup-gate-preflight.sh"
rm -rf "$TMP_DIR/preflight"
mkdir -p "$TMP_DIR/preflight/bin" "$TMP_DIR/preflight/state"
: > "$TMP_DIR/preflight/kubectl.log"

cat > "$TMP_DIR/preflight/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$TEST_LOG"
ns=""
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--namespace)
      ns="$2"
      shift 2
      ;;
    *)
      break
      ;;
  esac
done
cmd="$*"

case "$cmd" in
  "delete pod -l cube.tencent.com/pvm-preflight="*)
    rm -f "$TEST_STATE"/stale-*
    ;;
  "get pods -l cube.tencent.com/pvm-preflight="*"-o name")
    for f in "$TEST_STATE"/stale-*; do
      [ -e "$f" ] || continue
      printf 'pod/%s\n' "$(basename "$f")"
    done
    ;;
  "get --raw=/apis/apps.kruise.io/v1beta1"|"get --raw=/apis/apps.kruise.io/v1alpha1")
    ;;
  get\ deployment\ kruise-controller-manager\ -o\ jsonpath=*)
    case "$cmd" in
      *readyReplicas*) printf '%s' "${KRUISE_MANAGER_READY:-1}" ;;
      *tolerations*) printf '%s\n' "${KRUISE_MANAGER_TOLERATIONS:-|Exists|}" ;;
      *) printf 'unexpected get deployment: %s\n' "$cmd" >&2; exit 1 ;;
    esac
    ;;
  get\ daemonset\ kruise-daemon\ -o\ jsonpath=*)
    case "$cmd" in
      *desiredNumberScheduled*) printf '%s' "${KRUISE_DAEMON_DESIRED:-1}" ;;
      *numberReady*) printf '%s' "${KRUISE_DAEMON_READY:-1}" ;;
      *tolerations*) printf '%s\n' "${KRUISE_DAEMON_TOLERATIONS:-|Exists|}" ;;
      *) printf 'unexpected get daemonset: %s\n' "$cmd" >&2; exit 1 ;;
    esac
    ;;
  get\ nodes\ -l\ *\ -o\ name)
    if [ -f "$TEST_STATE/nodes" ]; then
      cat "$TEST_STATE/nodes"
    else
      printf 'node/node-a\n'
    fi
    ;;
  get\ node/*)
    node="${cmd#get node/}"
    node="${node%% *}"
    if [ -f "$TEST_STATE/effects-$node" ]; then
      case "$cmd" in
        *.effect*) cat "$TEST_STATE/effects-$node"; exit 0 ;;
        *.value*) cat "$TEST_STATE/values-$node" 2>/dev/null || true; exit 0 ;;
      esac
    fi
    case "$cmd" in
      *.effect*) [ -f "$TEST_STATE/effects" ] && cat "$TEST_STATE/effects" || true ;;
      *.value*) [ -f "$TEST_STATE/values" ] && cat "$TEST_STATE/values" || true ;;
      *) printf 'unexpected get node: %s\n' "$cmd" >&2; exit 1 ;;
    esac
    ;;
  "create -f -")
    manifest="$(cat)"
    printf '%s\n' "$manifest" > "$TEST_STATE/last-pod.yaml"
    name="$(printf '%s\n' "$manifest" | awk '/^  name: /{print $2; exit}')"
    creates=0
    [ -f "$TEST_STATE/create-count" ] && creates="$(cat "$TEST_STATE/create-count")"
    creates=$((creates + 1))
    printf '%s' "$creates" > "$TEST_STATE/create-count"
    phase="${CHECK_POD_PHASE:-Succeeded}"
    if [ -f "$TEST_STATE/phase-by-create" ]; then
      phase="$(awk -v n="$creates" 'NR==n {print; exit}' "$TEST_STATE/phase-by-create")"
      [ -n "$phase" ] || phase="${CHECK_POD_PHASE:-Succeeded}"
    fi
    printf '%s' "$phase" > "$TEST_STATE/phase-$name"
    : > "$TEST_STATE/created-$name"
    ;;
  wait\ *)
    # "wait --for=jsonpath=...=Succeeded pod/<name> --timeout=..."
    name=""
    for tok in $cmd; do
      case "$tok" in
        pod/*) name="${tok#pod/}" ;;
      esac
    done
    [ -n "$name" ] || { printf 'wait missing pod/: %s\n' "$cmd" >&2; exit 1; }
    if [ -f "$TEST_STATE/phase-$name" ]; then
      phase="$(cat "$TEST_STATE/phase-$name")"
    else
      phase="${CHECK_POD_PHASE:-Succeeded}"
    fi
    [ "$phase" = "Succeeded" ] || exit 1
    ;;
  taint\ node\ *)
    # "taint node <name> key=true:NoSchedule --overwrite"
    node="$(printf '%s\n' "$cmd" | awk '{print $3}')"
    printf 'NoSchedule ' > "$TEST_STATE/effects"
    printf 'NoSchedule ' > "$TEST_STATE/effects-$node"
    printf 'true ' > "$TEST_STATE/values"
    printf 'true ' > "$TEST_STATE/values-$node"
    : > "$TEST_STATE/tainted-$node"
    ;;
  get\ pod\ *\ -o\ jsonpath=*)
    # "get pod <name> -o jsonpath=..." (failure diagnostics after wait)
    name="$(printf '%s\n' "$cmd" | awk '{print $3}')"
    if [ -f "$TEST_STATE/phase-$name" ]; then
      cat "$TEST_STATE/phase-$name"
    else
      printf '%s' "${CHECK_POD_PHASE:-Succeeded}"
    fi
    ;;
  logs\ *)
    ;;
  delete\ pod\ *\ --wait=false)
    name="$(printf '%s\n' "$cmd" | awk '{print $3}')"
    rm -f "$TEST_STATE/phase-$name" "$TEST_STATE/created-$name"
    ;;
  *)
    printf 'unexpected kubectl (%sns=%s): %s\n' "" "${ns:-}" "$cmd" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$TMP_DIR/preflight/bin/kubectl"

export PATH="$TMP_DIR/preflight/bin:$PATH"
export TEST_LOG="$TMP_DIR/preflight/kubectl.log"
export TEST_STATE="$TMP_DIR/preflight/state"
export POD_NAMESPACE=test-ns
export NODE_SELECTOR=cube.tencent.com/pvm=true
export TAINT_KEY=cube.tencent.com/pvm-not-ready
export TAINT_EFFECT=NoSchedule
export PREFLIGHT_TIMEOUT_SECONDS=5
export RELEASE_NAME=test-release
export IS_UPGRADE=false
export CHECK_IMAGE=cube-pvm-host-bootstrap:test
export CHECK_IMAGE_PULL_POLICY=IfNotPresent
export PREFLIGHT_SERVICE_ACCOUNT=test-pvm-preflight
export IMAGE_PULL_SECRET_NAMES=
export STATE_DIR=/var/lib/cube-node-bootstrap
export DESIRED_KERNEL_PATTERN=pvm.host
export KERNEL_BOOT_ARGS="nopti pti=off"
export KRUISE_MANAGER_READY=1
export KRUISE_DAEMON_DESIRED=1
export KRUISE_DAEMON_READY=1
export KRUISE_MANAGER_TOLERATIONS='|Exists|'
export KRUISE_DAEMON_TOLERATIONS='|Exists|'
export CHECK_POD_PHASE=Succeeded

# Fingerprint-ready untainted node passes (no auto-taint).
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
sh "$PREFLIGHT_SCRIPT"
grep -q 'create -f -' "$TEST_LOG"
grep -q 'wait ' "$TEST_LOG"
grep -q 'value: "true"' "$TEST_STATE/last-pod.yaml"
if grep -q 'hostPID:' "$TEST_STATE/last-pod.yaml"; then
  echo "check pod unexpectedly set hostPID" >&2
  exit 1
fi
if grep -q 'taint node ' "$TEST_LOG"; then
  echo "fingerprint-ready path unexpectedly tainted" >&2
  exit 1
fi

# Untainted + fingerprint fail → auto-taint, then CNI probe succeeds.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
printf 'Failed\nSucceeded\n' > "$TEST_STATE/phase-by-create"
sh "$PREFLIGHT_SCRIPT"
grep -q 'taint node node-a cube.tencent.com/pvm-not-ready=true:NoSchedule --overwrite' "$TEST_LOG"
[ -f "$TEST_STATE/tainted-node-a" ]
grep -q 'value: "false"' "$TEST_STATE/last-pod.yaml"

# Gated node probes CNI without requiring fingerprint (no re-taint).
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
printf 'NoSchedule ' > "$TEST_STATE/effects"
sh "$PREFLIGHT_SCRIPT"
grep -q 'value: "false"' "$TEST_STATE/last-pod.yaml"
if grep -q 'taint node ' "$TEST_LOG"; then
  echo "already-gated path unexpectedly re-tainted" >&2
  exit 1
fi

# Upgrade with non-maintenance gate value fails.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
printf 'NoSchedule ' > "$TEST_STATE/effects"
printf 'true ' > "$TEST_STATE/values"
if (
  export IS_UPGRADE=true
  sh "$PREFLIGHT_SCRIPT"
); then
  echo "upgrade non-maintenance gate unexpectedly passed" >&2
  exit 1
fi

# Upgrade with maintenance gate value passes.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
printf 'NoSchedule ' > "$TEST_STATE/effects"
printf 'maintenance ' > "$TEST_STATE/values"
IS_UPGRADE=true sh "$PREFLIGHT_SCRIPT"

# Missing Kruise manager ready fails.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
if (
  export KRUISE_MANAGER_READY=0
  sh "$PREFLIGHT_SCRIPT"
); then
  echo "kruise manager not ready unexpectedly passed" >&2
  exit 1
fi

# Manager without gate toleration still passes (scheme A: Ready only).
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
KRUISE_MANAGER_TOLERATIONS='other|Equal|NoSchedule' sh "$PREFLIGHT_SCRIPT"

# Daemon without gate toleration fails.
if (
  export KRUISE_DAEMON_TOLERATIONS='other|Equal|NoSchedule'
  sh "$PREFLIGHT_SCRIPT"
); then
  echo "missing daemon toleration unexpectedly passed" >&2
  exit 1
fi

# No matching nodes fails.
if (
  : > "$TEST_STATE/nodes"
  sh "$PREFLIGHT_SCRIPT"
); then
  echo "empty node list unexpectedly passed" >&2
  exit 1
fi

# Stale check pods are cleaned before proceeding.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
: > "$TEST_STATE/stale-old"
sh "$PREFLIGHT_SCRIPT"
grep -q 'delete pod -l cube.tencent.com/pvm-preflight=test-release' "$TEST_LOG"

# Image pull secrets are rendered into the check pod.
: > "$TEST_LOG"
rm -f "$TEST_STATE"/*
IMAGE_PULL_SECRET_NAMES=regcred,othercred sh "$PREFLIGHT_SCRIPT"
grep -q 'imagePullSecrets:' "$TEST_STATE/last-pod.yaml"
grep -q 'name: regcred' "$TEST_STATE/last-pod.yaml"
grep -q 'name: othercred' "$TEST_STATE/last-pod.yaml"

echo "PVM startup-gate tests passed"
