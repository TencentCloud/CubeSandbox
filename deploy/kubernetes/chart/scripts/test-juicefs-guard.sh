#!/bin/sh
# Guard: JuiceFS is off by default; enabling it renders the format Job,
# per-node FUSE mount DaemonSet, and Cubelet configuration environment.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

expect_fail() {
  name="$1"
  needle="$2"
  shift 2
  if helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >/dev/null 2>"$TMP_DIR/$name.err"; then
    echo "expected fail: $name" >&2
    exit 1
  fi
  grep -qi "$needle" "$TMP_DIR/$name.err" || {
    echo "unexpected error for $name (wanted /$needle/):" >&2
    cat "$TMP_DIR/$name.err" >&2
    exit 1
  }
}

helm template jfs-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
if grep -q 'app.kubernetes.io/component: cube-juicefs' "$TMP_DIR/default.yaml"; then
  echo "default render must not include JuiceFS resources" >&2
  exit 1
fi
if grep -q 'name: CUBE_JFS_ENABLED' "$TMP_DIR/default.yaml"; then
  echo "default render must not enable Cubelet JuiceFS" >&2
  exit 1
fi

helm template jfs-on "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true > "$TMP_DIR/on.yaml"

grep -q 'app.kubernetes.io/component: cube-juicefs-format' "$TMP_DIR/on.yaml" \
  || { echo "enabled render must include the format Job" >&2; exit 1; }
grep -q 'app.kubernetes.io/component: cube-juicefs' "$TMP_DIR/on.yaml" \
  || { echo "enabled render must include the mount DaemonSet" >&2; exit 1; }
grep -q 'mountPropagation: Bidirectional' "$TMP_DIR/on.yaml" \
  || { echo "JuiceFS mount must propagate through data-cubelet" >&2; exit 1; }
grep -q 'path: /dev/fuse' "$TMP_DIR/on.yaml" \
  || { echo "JuiceFS mount must expose /dev/fuse" >&2; exit 1; }
grep -q 'type: OnDelete' "$TMP_DIR/on.yaml" \
  || { echo "JuiceFS mount updates must require an explicit drained-node delete" >&2; exit 1; }
grep -q 'name: wait-volume-format' "$TMP_DIR/on.yaml" \
  || { echo "JuiceFS mount must wait for the format Job" >&2; exit 1; }
grep -q 'name: CUBE_JFS_ENABLED' "$TMP_DIR/on.yaml" \
  || { echo "enabled render must configure Cubelet" >&2; exit 1; }
grep -q 'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1' "$TMP_DIR/on.yaml" \
  || { echo "enabled render must pull JuiceFS from TCR int" >&2; exit 1; }
grep -q 'command: \["/usr/local/bin/juicefs"\]' "$TMP_DIR/on.yaml" \
  || { echo "mount DaemonSet must invoke JuiceFS instead of the image default command" >&2; exit 1; }
grep -q 'app.kubernetes.io/component: cube-juicefs-redis' "$TMP_DIR/on.yaml" \
  || { echo "enabled render must include a dedicated JuiceFS Redis" >&2; exit 1; }
grep -q 'redis://jfs-on-cube-juicefs-redis.default.svc.cluster.local:6379/0' "$TMP_DIR/on.yaml" \
  || { echo "default metadata URL must use the dedicated JuiceFS Redis" >&2; exit 1; }
grep -q 'http://jfs-on-cube-minio.default.svc.cluster.local:9000/cube-jfs' "$TMP_DIR/on.yaml" \
  || { echo "default object bucket must use chart MinIO" >&2; exit 1; }
grep -q 'key: meta-password' "$TMP_DIR/on.yaml" \
  || { echo "default metadata password must use the JuiceFS Secret" >&2; exit 1; }
grep -q 'key: minio-root-password' "$TMP_DIR/on.yaml" \
  || { echo "default object credentials must use the chart Secret" >&2; exit 1; }
grep -q 'juicefs format --no-update' "$TMP_DIR/on.yaml" \
  || { echo "format Job must initialize JuiceFS idempotently" >&2; exit 1; }
grep -q 'juicefs status "$JFS_META_URL"' "$TMP_DIR/on.yaml" \
  || { echo "format Job must verify JuiceFS status" >&2; exit 1; }

helm template jfs-cube-external "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set redis.enabled=false \
  --set-string redis.host=cube-business-redis.example \
  > "$TMP_DIR/cube-external.yaml"
grep -q 'redis://jfs-cube-external-juicefs-redis.default.svc.cluster.local:6379/0' "$TMP_DIR/cube-external.yaml" \
  || { echo "JuiceFS metadata must stay on its dedicated Redis when Cube uses external Redis" >&2; exit 1; }

helm template jfs-external "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.url=redis://redis.example:6379/0 \
  --set-string cubeJuiceFS.metadata.existingSecret=jfs-meta \
  --set-string cubeJuiceFS.storage.bucket=https://bucket.example \
  --set-string cubeJuiceFS.storage.existingSecret=jfs-object \
  > "$TMP_DIR/external.yaml"
grep -q 'name: jfs-meta' "$TMP_DIR/external.yaml" \
  || { echo "external metadata Secret must be referenced" >&2; exit 1; }
grep -q 'name: jfs-object' "$TMP_DIR/external.yaml" \
  || { echo "external object Secret must be referenced" >&2; exit 1; }
if grep -q 'app.kubernetes.io/component: cube-juicefs-redis' "$TMP_DIR/external.yaml"; then
  echo "external metadata URL must skip the dedicated JuiceFS Redis" >&2
  exit 1
fi
helm template jfs-external "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.url=redis://redis.example:6379/0 \
  --set-string cubeJuiceFS.metadata.existingSecret=jfs-meta \
  --set-string cubeJuiceFS.metadata.secretRevision=rotation-2 \
  --set-string cubeJuiceFS.storage.bucket=https://bucket.example \
  --set-string cubeJuiceFS.storage.existingSecret=jfs-object \
  > "$TMP_DIR/external-revision.yaml"
base_hash="$(awk '/cube.tencent.com\/juicefs-config-hash:/ {print $2; exit}' "$TMP_DIR/external.yaml")"
revision_hash="$(awk '/cube.tencent.com\/juicefs-config-hash:/ {print $2; exit}' "$TMP_DIR/external-revision.yaml")"
if [ -z "$base_hash" ] || [ "$base_hash" = "$revision_hash" ]; then
  echo "metadata.secretRevision must change the JuiceFS lifecycle hash" >&2
  exit 1
fi

helm template jfs-mirror "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set-string global.imageRegistry=mirror.example.com > "$TMP_DIR/mirror.yaml"
grep -q 'mirror.example.com/cube-sandbox/juicefs:ce-v1.4.1' "$TMP_DIR/mirror.yaml" \
  || { echo "global.imageRegistry must rewrite the JuiceFS image to the mirror host" >&2; exit 1; }

helm template jfs-cn "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  -f "$CHART_DIR/values-cn.yaml" > "$TMP_DIR/cn.yaml"
grep -q 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1' "$TMP_DIR/cn.yaml" \
  || { echo "values-cn must render the JuiceFS image from the CN host" >&2; exit 1; }
grep -q 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/redis:7-alpine' "$TMP_DIR/cn.yaml" \
  || { echo "values-cn must render the JuiceFS metadata Redis from the CN host" >&2; exit 1; }
if grep -q 'juicedata/mount' "$TMP_DIR/on.yaml" "$TMP_DIR/mirror.yaml" "$TMP_DIR/cn.yaml"; then
  echo "JuiceFS must not be pulled from Docker Hub" >&2
  exit 1
fi

helm template jfs-on "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set-string images.juicefs.pullPolicy=Always > "$TMP_DIR/changed-pod.yaml"
base_job="$(sed -n '/^kind: Job$/{n; /metadata:/!b; n; s/^  name: //p; q;}' "$TMP_DIR/on.yaml")"
changed_job="$(sed -n '/^kind: Job$/{n; /metadata:/!b; n; s/^  name: //p; q;}' "$TMP_DIR/changed-pod.yaml")"
if [ -z "$base_job" ] || [ -z "$changed_job" ] || [ "$base_job" = "$changed_job" ]; then
  echo "format Job name must change when its immutable Pod spec changes" >&2
  exit 1
fi

expect_fail jfs-bad-path "absolute child of hostPaths.dataCubelet" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.mountPath=/mnt/jfs
expect_fail jfs-overlap "must not overlap" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.cacheDir=/data/cubelet/storage/jfs/cache
expect_fail jfs-bad-root "clean relative path" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.root=..
expect_fail jfs-no-minio "storage.bucket is required" \
  --set cubeJuiceFS.enabled=true \
  --set minio.enabled=false
expect_fail jfs-external-no-creds "requires storage.existingSecret" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.storage.bucket=https://bucket.example

helm template jfs-db-zero "$CHART_DIR" $COMMON_SETS \
  --set cubeJuiceFS.enabled=true \
  --set cubeJuiceFS.metadata.database=0 > "$TMP_DIR/db-zero.yaml"
grep -q 'redis://jfs-db-zero-cube-juicefs-redis.default.svc.cluster.local:6379/0' "$TMP_DIR/db-zero.yaml" \
  || { echo "metadata.database=0 must remain database 0" >&2; exit 1; }
expect_fail jfs-db-not-integer "must be an integer in 0..15" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.database=invalid
expect_fail jfs-db-out-of-range "must be an integer in 0..15" \
  --set cubeJuiceFS.enabled=true \
  --set cubeJuiceFS.metadata.database=16

expect_fail jfs-managed-existing-secret "only supported with an external metadata.url" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.existingSecret=jfs-meta
expect_fail jfs-url-password "must not embed a password" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.url=redis://:secret@redis.example:6379/0
expect_fail jfs-redis-persistence-ambiguous "mutually exclusive" \
  --set cubeJuiceFS.enabled=true \
  --set-string cubeJuiceFS.metadata.redis.persistence.hostPath=/data/jfs-redis \
  --set-string cubeJuiceFS.metadata.redis.persistence.existingClaim=jfs-redis

echo "juicefs chart guard passed"
