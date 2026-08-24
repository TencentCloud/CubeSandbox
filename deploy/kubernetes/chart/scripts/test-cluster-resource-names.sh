#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
release_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
release_b=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab

render() {
  release="$1"
  namespace="$2"
  output="$3"
  shift 3
  helm template "$release" "$CHART_DIR" -n "$namespace" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

# eviction-webhook is opt-in; also verify its cluster-scoped resources do not
# collide with the rest of the chart when enabled.
render_webhook() {
  release="$1"
  namespace="$2"
  output="$3"
  render "$release" "$namespace" "$output" --set evictionWebhook.enabled=true
}

cluster_names() {
  awk '
    /^kind: (PriorityClass|ClusterRole|ClusterRoleBinding|ValidatingWebhookConfiguration)$/ {kind=$2; cluster=1; next}
    cluster && /^  name: / {print kind "/" $2; cluster=0}
    /^---$/ {cluster=0}
  ' "$1" | sort
}

render "$release_a" namespace-a "$TMP_DIR/a.yaml"
render "$release_a" namespace-b "$TMP_DIR/b.yaml"
render "$release_b" namespace-a "$TMP_DIR/c.yaml"
# eviction-webhook is opt-in; render it on top of the default chart to verify
# its cluster-scoped resources (ClusterRole/Binding, VWC) stay release+namespace
# scoped and do not collide with anything the chart already creates.
render_webhook "$release_a" namespace-a "$TMP_DIR/w.yaml"
cluster_names "$TMP_DIR/a.yaml" > "$TMP_DIR/a.names"
cluster_names "$TMP_DIR/b.yaml" > "$TMP_DIR/b.names"
cluster_names "$TMP_DIR/c.yaml" > "$TMP_DIR/c.names"
cluster_names "$TMP_DIR/w.yaml" > "$TMP_DIR/w.names"

[ ! -s "$TMP_DIR/a.names" ] && { echo "no cluster-scoped resources found" >&2; exit 1; }
[ -z "$(uniq -d "$TMP_DIR/a.names")" ] || {
  echo "cluster-scoped names collide within one release" >&2
  exit 1
}
[ -z "$(uniq -d "$TMP_DIR/w.names")" ] || {
  echo "cluster-scoped names collide within the eviction-webhook-enabled render" >&2
  exit 1
}
[ -z "$(comm -12 "$TMP_DIR/a.names" "$TMP_DIR/b.names")" ] || {
  echo "same long release name collides across namespaces" >&2
  exit 1
}
[ -z "$(comm -12 "$TMP_DIR/a.names" "$TMP_DIR/c.names")" ] || {
  echo "different long release names collide within one namespace" >&2
  exit 1
}
[ -z "$(comm -12 "$TMP_DIR/w.names" "$TMP_DIR/b.names")" ] || {
  echo "eviction-webhook cluster-scoped names are not namespace-scoped" >&2
  exit 1
}
[ -z "$(comm -12 "$TMP_DIR/w.names" "$TMP_DIR/c.names")" ] || {
  echo "eviction-webhook cluster-scoped names are not release-scoped" >&2
  exit 1
}
echo "Cluster-scoped resource name guard passed"
