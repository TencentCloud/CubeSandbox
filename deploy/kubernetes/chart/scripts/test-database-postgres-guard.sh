#!/bin/sh
# Guard: database.driver selects mysql.* or postgres.* without cross-section
# inference. postgres requires an external postgres.host, skips the built-in
# MySQL StatefulSet, sets the CubeMaster driver, and feeds CubeOps/CubeAPI
# DATABASE_URL from a Secret (not a plaintext Deployment env value).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

extract_component_doc() {
  component="$1"
  input="$2"
  output="$3"
  awk -v component="$component" '
    BEGIN { RS="\n---\n"; ORS="\n---\n" }
    index($0, "app.kubernetes.io/component: " component) { print; found=1 }
    END { exit found ? 0 : 1 }
  ' "$input" > "$output"
}

extract_kind_doc() {
  kind="$1"
  input="$2"
  output="$3"
  awk -v kind="$kind" '
    BEGIN { RS="\n---\n"; ORS="\n---\n" }
    $0 ~ ("kind: " kind) { print; found=1 }
    END { exit found ? 0 : 1 }
  ' "$input" > "$output"
}

# Default mysql path still works and keeps MYSQL_* for CubeOps.
helm template db-driver-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
grep -q 'driver: "mysql"' "$TMP_DIR/default.yaml" || {
  echo "default render missing driver: mysql in CubeMaster config" >&2
  exit 1
}
extract_component_doc ops "$TMP_DIR/default.yaml" "$TMP_DIR/default-ops.yaml"
grep -q 'CUBE_SANDBOX_MYSQL_HOST' "$TMP_DIR/default-ops.yaml" || {
  echo "default render missing CUBE_SANDBOX_MYSQL_HOST for CubeOps" >&2
  exit 1
}
grep -q 'kind: StatefulSet' "$TMP_DIR/default.yaml" || {
  echo "default render should keep built-in MySQL StatefulSet" >&2
  exit 1
}

# External MySQL through mysql.host keeps working (pre-existing values files).
helm template db-driver-ext-mysql "$CHART_DIR" \
  --set-string redis.password=test \
  --set-string mysql.host=10.0.0.11 \
  --set mysql.port=3307 \
  --set-string mysql.user=cube \
  --set-string mysql.password=extmysql \
  --set-string mysql.database=cube_mvp \
  > "$TMP_DIR/ext-mysql.yaml"
grep -q '10.0.0.11:3307' "$TMP_DIR/ext-mysql.yaml" || {
  echo "external mysql.host/port not applied" >&2
  exit 1
}
if awk '
  BEGIN { want=0 }
  /^kind: StatefulSet$/ { want=1; next }
  want && /app.kubernetes.io\/component: mysql/ { found=1 }
  /^---$/ { want=0 }
  END { exit found ? 0 : 1 }
' "$TMP_DIR/ext-mysql.yaml"; then
  echo "external mysql.host must skip built-in MySQL StatefulSet" >&2
  exit 1
fi
extract_kind_doc Secret "$TMP_DIR/ext-mysql.yaml" "$TMP_DIR/ext-mysql-secret.yaml"
if grep -q 'mysql-root-password' "$TMP_DIR/ext-mysql-secret.yaml"; then
  echo "external mysql.host must not emit an unused mysql-root-password sentinel" >&2
  exit 1
fi

# postgres without postgres.host must fail.
if helm template db-driver-pg-nohost "$CHART_DIR" $COMMON_SETS \
  --set-string database.driver=postgres >/dev/null 2>"$TMP_DIR/pg-nohost.err"; then
  echo "expected fail when database.driver=postgres without postgres.host" >&2
  exit 1
fi
grep -qi 'database.driver=postgres requires postgres.host' "$TMP_DIR/pg-nohost.err" || {
  echo "unexpected error for postgres without host:" >&2
  cat "$TMP_DIR/pg-nohost.err" >&2
  exit 1
}

# mysql.host must not satisfy the postgres driver (no cross-section inference).
if helm template db-driver-pg-crossref "$CHART_DIR" $COMMON_SETS \
  --set-string database.driver=postgres \
  --set-string mysql.host=10.0.0.12 >/dev/null 2>"$TMP_DIR/pg-crossref.err"; then
  echo "expected fail when only mysql.host is set for the postgres driver" >&2
  exit 1
fi
grep -qi 'requires postgres.host' "$TMP_DIR/pg-crossref.err" || {
  echo "unexpected error for postgres with only mysql.host:" >&2
  cat "$TMP_DIR/pg-crossref.err" >&2
  exit 1
}

# invalid driver must fail.
if helm template db-driver-bad "$CHART_DIR" $COMMON_SETS \
  --set-string database.driver=sqlite >/dev/null 2>"$TMP_DIR/bad.err"; then
  echo "expected fail for invalid database.driver" >&2
  exit 1
fi
grep -qi 'database.driver must be mysql or postgres' "$TMP_DIR/bad.err" || {
  echo "unexpected error for invalid driver:" >&2
  cat "$TMP_DIR/bad.err" >&2
  exit 1
}

# external mysql.host with the CHANGE_ME password must fail.
if helm template db-driver-mysql-changeme "$CHART_DIR" \
  --set-string redis.password=test \
  --set-string mysql.host=10.0.0.10 >/dev/null 2>"$TMP_DIR/mysql-changeme.err"; then
  echo "expected fail when mysql.host is set with CHANGE_ME password" >&2
  exit 1
fi
grep -qi 'CHANGE_ME' "$TMP_DIR/mysql-changeme.err" || {
  echo "unexpected error for CHANGE_ME with external mysql.host:" >&2
  cat "$TMP_DIR/mysql-changeme.err" >&2
  exit 1
}

# postgres.host with the CHANGE_ME password must fail.
if helm template db-driver-pg-changeme "$CHART_DIR" $COMMON_SETS \
  --set-string database.driver=postgres \
  --set-string postgres.host=10.0.0.10 >/dev/null 2>"$TMP_DIR/pg-changeme.err"; then
  echo "expected fail when postgres.host is set with CHANGE_ME password" >&2
  exit 1
fi
grep -qi 'CHANGE_ME' "$TMP_DIR/pg-changeme.err" || {
  echo "unexpected error for CHANGE_ME with external postgres.host:" >&2
  cat "$TMP_DIR/pg-changeme.err" >&2
  exit 1
}

# postgres + host: no built-in MySQL, CubeMaster driver=postgres,
# CubeOps/CubeAPI DATABASE_URL via Secret; port defaults to 5432.
helm template db-driver-pg "$CHART_DIR" $COMMON_SETS \
  --set-string database.driver=postgres \
  --set-string postgres.host=10.0.0.10 \
  --set-string postgres.user=postgres \
  --set-string postgres.password=test \
  --set-string postgres.database=cube_mvp \
  > "$TMP_DIR/pg.yaml"

grep -q 'driver: "postgres"' "$TMP_DIR/pg.yaml" || {
  echo "postgres render missing driver: postgres in CubeMaster config" >&2
  exit 1
}
extract_component_doc ops "$TMP_DIR/pg.yaml" "$TMP_DIR/pg-ops.yaml"
grep -q 'name: DATABASE_URL' "$TMP_DIR/pg-ops.yaml" || {
  echo "postgres render missing DATABASE_URL for CubeOps" >&2
  exit 1
}
grep -q 'key: database-url' "$TMP_DIR/pg-ops.yaml" || {
  echo "postgres CubeOps DATABASE_URL must use secretKeyRef key database-url" >&2
  exit 1
}
if grep -q 'postgresql://' "$TMP_DIR/pg-ops.yaml"; then
  echo "postgres CubeOps must not embed DATABASE_URL password in Deployment env" >&2
  exit 1
fi
if grep -q 'CUBE_SANDBOX_MYSQL_HOST' "$TMP_DIR/pg-ops.yaml"; then
  echo "postgres CubeOps must not set CUBE_SANDBOX_MYSQL_*" >&2
  exit 1
fi
extract_component_doc api "$TMP_DIR/pg.yaml" "$TMP_DIR/pg-api.yaml"
grep -q 'key: database-url' "$TMP_DIR/pg-api.yaml" || {
  echo "postgres CubeAPI DATABASE_URL must use secretKeyRef key database-url" >&2
  exit 1
}
if grep -q 'CUBE_SANDBOX_MYSQL_HOST' "$TMP_DIR/pg-api.yaml"; then
  echo "postgres CubeAPI must not set CUBE_SANDBOX_MYSQL_*" >&2
  exit 1
fi
extract_kind_doc Secret "$TMP_DIR/pg.yaml" "$TMP_DIR/pg-secret.yaml"
grep -q 'database-url: "postgresql://postgres:test@10.0.0.10:5432/cube_mvp"' "$TMP_DIR/pg-secret.yaml" || {
  echo "postgres Secret missing expected database-url value (default port 5432)" >&2
  exit 1
}
# mysql-only keys would be misleading here: nothing reads them, and
# mysql-root-password would carry the untouched CHANGE_ME_* sentinel.
if grep -qE 'mysql-password|mysql-root-password' "$TMP_DIR/pg-secret.yaml"; then
  echo "postgres Secret must not emit mysql-password / mysql-root-password" >&2
  exit 1
fi
# Built-in MySQL StatefulSet uses component mysql; ensure it is absent.
if awk '
  BEGIN { want=0 }
  /^kind: StatefulSet$/ { want=1; next }
  want && /app.kubernetes.io\/component: mysql/ { found=1 }
  /^---$/ { want=0 }
  END { exit found ? 0 : 1 }
' "$TMP_DIR/pg.yaml"; then
  echo "postgres render must skip built-in MySQL StatefulSet" >&2
  exit 1
fi

echo "ok: default mysql driver keeps MYSQL_* and built-in StatefulSet"
echo "ok: external mysql.host keeps working and skips the built-in StatefulSet"
echo "ok: postgres without postgres.host / mysql.host only / invalid driver / CHANGE_ME fail as expected"
echo "ok: postgres + host sets CubeMaster/CubeOps/CubeAPI DATABASE_URL via Secret (port 5432 default)"
echo "All database.driver guard tests passed"
