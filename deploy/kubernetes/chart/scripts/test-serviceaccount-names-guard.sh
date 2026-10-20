#!/bin/sh
# Guard: per-component ServiceAccount name resolution, annotation propagation,
# disabled-component suppression, and cubeProxy.hostUsers rendering.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

render() {
  output="$1"; shift
  helm template sa-guard "$CHART_DIR" $COMMON_SETS "$@" > "$output"
}
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok()   { printf 'ok: %s\n'   "$*"; }

# ---------------------------------------------------------------------------
# Extraction helpers
# ---------------------------------------------------------------------------

# sa_name_from_workload <rendered.yaml> <component-label-value>
# Prints the serviceAccountName from the first DaemonSet/Deployment bearing
# that app.kubernetes.io/component label, or "" when the field is absent.
sa_name_from_workload() {
  python3 - "$1" "$2" <<'PY'
import pathlib, re, sys

text   = pathlib.Path(sys.argv[1]).read_text()
comp   = sys.argv[2]
docs   = text.split("\n---\n")

for doc in docs:
    body = f"\n{doc}\n"
    if not re.search(r"\nkind: (DaemonSet|Deployment)\n", body):
        continue
    if f"\n    app.kubernetes.io/component: {comp}\n" not in body:
        continue
    m = re.search(r"(?m)^ {6}serviceAccountName:\s*(\S+)\s*$", doc)
    sys.stdout.write((m.group(1) if m else "") + "\n")
    sys.exit(0)

raise SystemExit(f"no DaemonSet/Deployment found for component '{comp}'")
PY
}

# sa_resource_name <rendered.yaml> <component-label-value>
# Prints the metadata.name of the ServiceAccount bearing that component label,
# or "" when no such ServiceAccount exists in the rendered output.
sa_resource_name() {
  python3 - "$1" "$2" <<'PY'
import pathlib, re, sys

text = pathlib.Path(sys.argv[1]).read_text()
comp = sys.argv[2]
docs = text.split("\n---\n")

for doc in docs:
    body = f"\n{doc}\n"
    if "\nkind: ServiceAccount\n" not in body:
        continue
    if f"\n    app.kubernetes.io/component: {comp}\n" not in body:
        continue
    m = re.search(r"(?m)^  name:\s*(\S+)\s*$", doc)
    sys.stdout.write((m.group(1) if m else "") + "\n")
    sys.exit(0)

sys.stdout.write("\n")  # not found -> empty
PY
}

# extract_sa_doc <rendered.yaml> <component-label-value> <output-file>
# Writes the full ServiceAccount doc for that component to <output-file>,
# or an empty file when no such ServiceAccount exists in the rendered output.
extract_sa_doc() {
  python3 - "$1" "$2" "$3" <<'PY'
import pathlib, sys

text = pathlib.Path(sys.argv[1]).read_text()
comp = sys.argv[2]
out  = pathlib.Path(sys.argv[3])
for doc in text.split("\n---\n"):
    body = f"\n{doc}\n"
    if "\nkind: ServiceAccount\n" not in body:
        continue
    if f"\n    app.kubernetes.io/component: {comp}\n" not in body:
        continue
    out.write_text(doc + "\n")
    sys.exit(0)
out.write_text("")  # not found -> empty file
PY
}

# ===========================================================================
# cube-node  (top-level serviceAccount)
# ===========================================================================

# create=true, name="" -> <release>-node
render "$TMP_DIR/node-default.yaml"
got="$(sa_name_from_workload "$TMP_DIR/node-default.yaml" cube-node)"
[ "$got" = "sa-guard-cube-node" ] \
  || fail "node create=true name='': expected sa-guard-cube-node, got '$got'"
res="$(sa_resource_name "$TMP_DIR/node-default.yaml" cube-node)"
[ "$res" = "sa-guard-cube-node" ] \
  || fail "node SA resource create=true name='': expected sa-guard-cube-node, got '$res'"
ok "node create=true  name=''       -> sa-guard-cube-node"

# create=true, name=custom -> custom
render "$TMP_DIR/node-named.yaml" \
  --set-string serviceAccount.name=my-node-sa
got="$(sa_name_from_workload "$TMP_DIR/node-named.yaml" cube-node)"
[ "$got" = "my-node-sa" ] \
  || fail "node create=true name=custom: expected my-node-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/node-named.yaml" cube-node)"
[ "$res" = "my-node-sa" ] \
  || fail "node SA resource create=true name=custom: expected my-node-sa, got '$res'"
ok "node create=true  name=custom   -> my-node-sa"

# create=false, name="" -> default
render "$TMP_DIR/node-nocreate.yaml" \
  --set serviceAccount.create=false
got="$(sa_name_from_workload "$TMP_DIR/node-nocreate.yaml" cube-node)"
[ "$got" = "default" ] \
  || fail "node create=false name='': expected default, got '$got'"
res="$(sa_resource_name "$TMP_DIR/node-nocreate.yaml" cube-node)"
[ -z "$res" ] \
  || fail "node SA resource must not be created when create=false, got '$res'"
ok "node create=false name=''       -> default (no SA resource)"

# create=false, name=custom -> custom
render "$TMP_DIR/node-byo.yaml" \
  --set serviceAccount.create=false \
  --set-string serviceAccount.name=existing-node-sa
got="$(sa_name_from_workload "$TMP_DIR/node-byo.yaml" cube-node)"
[ "$got" = "existing-node-sa" ] \
  || fail "node create=false name=custom: expected existing-node-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/node-byo.yaml" cube-node)"
[ -z "$res" ] \
  || fail "node SA resource must not be created when create=false, got '$res'"
ok "node create=false name=custom   -> existing-node-sa (no SA resource)"

# ===========================================================================
# cube-proxy  (cubeProxy.serviceAccount)
# ===========================================================================

# create=true, name="" -> <release>-proxy
render "$TMP_DIR/proxy-default.yaml" \
  --set cubeProxy.serviceAccount.create=true
got="$(sa_name_from_workload "$TMP_DIR/proxy-default.yaml" cube-proxy)"
[ "$got" = "sa-guard-cube-proxy" ] \
  || fail "proxy create=true name='': expected sa-guard-cube-proxy, got '$got'"
res="$(sa_resource_name "$TMP_DIR/proxy-default.yaml" cube-proxy)"
[ "$res" = "sa-guard-cube-proxy" ] \
  || fail "proxy SA resource create=true name='': expected sa-guard-cube-proxy, got '$res'"
ok "proxy create=true  name=''       -> sa-guard-cube-proxy"

# create=true, name=custom -> custom
render "$TMP_DIR/proxy-named.yaml" \
  --set cubeProxy.serviceAccount.create=true \
  --set-string cubeProxy.serviceAccount.name=my-proxy-sa
got="$(sa_name_from_workload "$TMP_DIR/proxy-named.yaml" cube-proxy)"
[ "$got" = "my-proxy-sa" ] \
  || fail "proxy create=true name=custom: expected my-proxy-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/proxy-named.yaml" cube-proxy)"
[ "$res" = "my-proxy-sa" ] \
  || fail "proxy SA resource create=true name=custom: expected my-proxy-sa, got '$res'"
ok "proxy create=true  name=custom   -> my-proxy-sa"

# create=false, name="" -> no serviceAccountName field in Deployment
render "$TMP_DIR/proxy-nocreate.yaml"
got="$(sa_name_from_workload "$TMP_DIR/proxy-nocreate.yaml" cube-proxy)"
[ -z "$got" ] \
  || fail "proxy create=false name='': expected no serviceAccountName, got '$got'"
res="$(sa_resource_name "$TMP_DIR/proxy-nocreate.yaml" cube-proxy)"
[ -z "$res" ] \
  || fail "proxy SA resource must not be created when create=false, got '$res'"
ok "proxy create=false name=''       -> (no serviceAccountName, no SA resource)"

# create=false, name=custom -> custom serviceAccountName, no SA resource
render "$TMP_DIR/proxy-byo.yaml" \
  --set cubeProxy.serviceAccount.create=false \
  --set-string cubeProxy.serviceAccount.name=existing-proxy-sa
got="$(sa_name_from_workload "$TMP_DIR/proxy-byo.yaml" cube-proxy)"
[ "$got" = "existing-proxy-sa" ] \
  || fail "proxy create=false name=custom: expected existing-proxy-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/proxy-byo.yaml" cube-proxy)"
[ -z "$res" ] \
  || fail "proxy SA resource must not be created when create=false, got '$res'"
ok "proxy create=false name=custom   -> existing-proxy-sa (no SA resource)"

# annotations land on the SA resource and nowhere else
render "$TMP_DIR/proxy-ann.yaml" \
  --set cubeProxy.serviceAccount.create=true \
  --set-string "cubeProxy.serviceAccount.annotations.iam\.gke\.io/gsa-email=proxy@project.iam.gserviceaccount.com"
extract_sa_doc "$TMP_DIR/proxy-ann.yaml" cube-proxy "$TMP_DIR/proxy-ann-sa.yaml"
[ -s "$TMP_DIR/proxy-ann-sa.yaml" ] \
  || fail "proxy SA resource missing when create=true with annotations"
grep -q 'proxy@project.iam.gserviceaccount.com' "$TMP_DIR/proxy-ann-sa.yaml" \
  || fail "proxy SA annotation value not found in SA doc"
extract_sa_doc "$TMP_DIR/proxy-ann.yaml" cube-node "$TMP_DIR/proxy-ann-node-sa.yaml"
if grep -q 'proxy@project.iam.gserviceaccount.com' "$TMP_DIR/proxy-ann-node-sa.yaml"; then
  fail "proxy annotation must not appear in cube-node SA"
fi
ok "proxy annotations -> SA resource only, no bleed to other SAs"

# cubeProxy.enabled=false -> no SA even when create=true
render "$TMP_DIR/proxy-disabled.yaml" \
  --set cubeProxy.enabled=false \
  --set cubeProxy.serviceAccount.create=true
res="$(sa_resource_name "$TMP_DIR/proxy-disabled.yaml" cube-proxy)"
[ -z "$res" ] \
  || fail "proxy SA must not be rendered when cubeProxy.enabled=false, got '$res'"
ok "proxy disabled -> SA not rendered (create=true ignored)"

# ===========================================================================
# cube-master  (controlPlane.master.serviceAccount)
# ===========================================================================

# create=true, name="" -> <release>-master
render "$TMP_DIR/master-default.yaml" \
  --set controlPlane.master.serviceAccount.create=true
got="$(sa_name_from_workload "$TMP_DIR/master-default.yaml" master)"
[ "$got" = "sa-guard-cube-master" ] \
  || fail "master create=true name='': expected sa-guard-cube-master, got '$got'"
res="$(sa_resource_name "$TMP_DIR/master-default.yaml" master)"
[ "$res" = "sa-guard-cube-master" ] \
  || fail "master SA resource create=true name='': expected sa-guard-cube-master, got '$res'"
ok "master create=true  name=''       -> sa-guard-cube-master"

# create=true, name=custom -> custom
render "$TMP_DIR/master-named.yaml" \
  --set controlPlane.master.serviceAccount.create=true \
  --set-string controlPlane.master.serviceAccount.name=my-master-sa
got="$(sa_name_from_workload "$TMP_DIR/master-named.yaml" master)"
[ "$got" = "my-master-sa" ] \
  || fail "master create=true name=custom: expected my-master-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/master-named.yaml" master)"
[ "$res" = "my-master-sa" ] \
  || fail "master SA resource create=true name=custom: expected my-master-sa, got '$res'"
ok "master create=true  name=custom   -> my-master-sa"

# create=false, name="" -> no serviceAccountName field in Deployment
render "$TMP_DIR/master-nocreate.yaml"
got="$(sa_name_from_workload "$TMP_DIR/master-nocreate.yaml" master)"
[ -z "$got" ] \
  || fail "master create=false name='': expected no serviceAccountName, got '$got'"
res="$(sa_resource_name "$TMP_DIR/master-nocreate.yaml" master)"
[ -z "$res" ] \
  || fail "master SA resource must not be created when create=false, got '$res'"
ok "master create=false name=''       -> (no serviceAccountName, no SA resource)"

# create=false, name=custom -> custom serviceAccountName, no SA resource
render "$TMP_DIR/master-byo.yaml" \
  --set controlPlane.master.serviceAccount.create=false \
  --set-string controlPlane.master.serviceAccount.name=existing-master-sa
got="$(sa_name_from_workload "$TMP_DIR/master-byo.yaml" master)"
[ "$got" = "existing-master-sa" ] \
  || fail "master create=false name=custom: expected existing-master-sa, got '$got'"
res="$(sa_resource_name "$TMP_DIR/master-byo.yaml" master)"
[ -z "$res" ] \
  || fail "master SA resource must not be created when create=false, got '$res'"
ok "master create=false name=custom   -> existing-master-sa (no SA resource)"

# annotations land on the SA resource and nowhere else
render "$TMP_DIR/master-ann.yaml" \
  --set controlPlane.master.serviceAccount.create=true \
  --set-string "controlPlane.master.serviceAccount.annotations.example\.com/test=master"
extract_sa_doc "$TMP_DIR/master-ann.yaml" master "$TMP_DIR/master-ann-sa.yaml"
[ -s "$TMP_DIR/master-ann-sa.yaml" ] \
  || fail "master SA resource missing when create=true with annotations"
grep -q 'example.com/test: master' "$TMP_DIR/master-ann-sa.yaml" \
  || fail "master SA annotation not found in SA doc"
extract_sa_doc "$TMP_DIR/master-ann.yaml" cube-node "$TMP_DIR/master-ann-node-sa.yaml"
if grep -q 'example.com/test' "$TMP_DIR/master-ann-node-sa.yaml"; then
  fail "master annotation must not appear in cube-node SA"
fi
ok "master annotations -> SA resource only, no bleed to other SAs"

# controlPlane.enabled=false -> no SA even when create=true.
render "$TMP_DIR/master-disabled.yaml" \
  --set cubeNode.enabled=false \
  --set controlPlane.enabled=false \
  --set controlPlane.master.serviceAccount.create=true
res="$(sa_resource_name "$TMP_DIR/master-disabled.yaml" master)"
[ -z "$res" ] \
  || fail "master SA must not be rendered when controlPlane.enabled=false, got '$res'"
ok "master disabled -> SA not rendered (create=true ignored)"

# ===========================================================================
# cubeProxy.hostUsers
# ===========================================================================

# hostUsers=null (default) -> field absent
render "$TMP_DIR/hu-default.yaml"
if grep -q 'hostUsers:' "$TMP_DIR/hu-default.yaml"; then
  fail "hostUsers must be absent when cubeProxy.hostUsers=null (default)"
fi
ok "proxy hostUsers=null (default) -> field absent"

# hostUsers=false -> field present and false
render "$TMP_DIR/hu-false.yaml" --set cubeProxy.hostUsers=false
grep -q 'hostUsers: false' "$TMP_DIR/hu-false.yaml" \
  || fail "expected 'hostUsers: false' in proxy Deployment"
ok "proxy hostUsers=false -> hostUsers: false"

# hostUsers=true -> field present and true
render "$TMP_DIR/hu-true.yaml" --set cubeProxy.hostUsers=true
grep -q 'hostUsers: true' "$TMP_DIR/hu-true.yaml" \
  || fail "expected 'hostUsers: true' in proxy Deployment"
ok "proxy hostUsers=true -> hostUsers: true"

echo "ServiceAccount name guard passed"
