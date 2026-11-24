#!/bin/sh
# Guard: volumePlugins.extra renders into CubeMaster conf.yaml and the cubelet
# CUBE_EXTRA_VOLUME_PLUGINS env, and every invalid entry fails helm template
# (also with cubeNode.enabled=false), instead of crash-looping CubeMaster or
# the cubelets at startup.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

render() {
  helm template vp "$CHART_DIR" $COMMON_SETS "$@"
}

values() {
  # values <name>: write stdin as a values file and print its path.
  cat >"$TMP_DIR/values-$1.yaml"
  printf '%s\n' "$TMP_DIR/values-$1.yaml"
}

expect_fail() {
  name="$1"
  needle="$2"
  shift 2
  if render "$@" >/dev/null 2>"$TMP_DIR/$name.err"; then
    echo "expected helm template to fail: $name" >&2
    exit 1
  fi
  grep -q -- "$needle" "$TMP_DIR/$name.err" || {
    echo "unexpected error for $name (wanted /$needle/):" >&2
    cat "$TMP_DIR/$name.err" >&2
    exit 1
  }
}

render >"$TMP_DIR/default.yaml"
if grep -q 'CUBE_EXTRA_VOLUME_PLUGINS' "$TMP_DIR/default.yaml"; then
  echo "default render must not set CUBE_EXTRA_VOLUME_PLUGINS" >&2
  exit 1
fi
if grep -q 'Third-party plugin from values' "$TMP_DIR/default.yaml"; then
  echo "default render must not add CubeMaster volume_plugins entries" >&2
  exit 1
fi

GOOD="$(values good <<'YAML'
volumePlugins:
  extra:
    - name: myfs
      type: rpc
      master:
        socketPath: myfs-controller.myfs.svc:9200
      node:
        socketPath: /data/cube-shared/run/myfs/node.sock
    - name: tool_2
      type: binary
      master:
        binaryPath: /opt/custom/cube-volume-tool
      node:
        binaryPath: /data/cube-shared/bin/cube-volume-tool
YAML
)"
render -f "$GOOD" >"$TMP_DIR/good.yaml"
grep -q 'value: "myfs|rpc|/data/cube-shared/run/myfs/node.sock;tool_2|binary|/data/cube-shared/bin/cube-volume-tool"' "$TMP_DIR/good.yaml" || {
  echo "cubelet env must list both plugins with their node paths" >&2
  exit 1
}
grep -q 'socket_path: "myfs-controller.myfs.svc:9200"' "$TMP_DIR/good.yaml" || {
  echo "CubeMaster conf must register myfs with its master socketPath" >&2
  exit 1
}
grep -q 'binary_path: "/opt/custom/cube-volume-tool"' "$TMP_DIR/good.yaml" || {
  echo "CubeMaster conf must register tool_2 with its master binaryPath" >&2
  exit 1
}
# The packaged s3 plugin stays first, so it remains the default driver.
first="$(grep -A1 '^ *volume_plugins:' "$TMP_DIR/good.yaml" | grep -o 'name: [a-z0-9"_-]*' | head -n 1)"
[ "$first" = "name: s3" ] || {
  echo "s3 must stay the first CubeMaster volume plugin, got: $first" >&2
  exit 1
}

render -f "$GOOD" --set cubeNode.enabled=false >"$TMP_DIR/no-node.yaml"
grep -q 'socket_path: "myfs-controller.myfs.svc:9200"' "$TMP_DIR/no-node.yaml" || {
  echo "cubeNode.enabled=false must still register the master side" >&2
  exit 1
}

bad() {
  # bad <name> <needle> <yaml for the single extra entry, indented 4>
  name="$1"
  needle="$2"
  f="$(printf 'volumePlugins:\n  extra:\n%s\n' "$3" | values "$name")"
  expect_fail "$name" "$needle" -f "$f"
  expect_fail "$name-no-node" "$needle" -f "$f" --set cubeNode.enabled=false
}

RPC_SIDES='      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: /data/cube-shared/run/x.sock'

bad missing-name 'extra\[0\].name is required' "    - type: rpc
$RPC_SIDES"
bad bad-name 'name must match' "    - name: my.fs
      type: rpc
$RPC_SIDES"
bad reserved-s3 'is a packaged plugin' "    - name: s3
      type: rpc
$RPC_SIDES"
bad reserved-cos 'is a packaged plugin' "    - name: cos
      type: rpc
$RPC_SIDES"
bad duplicate 'already used by volumePlugins.extra\[0\]' "    - name: x
      type: rpc
$RPC_SIDES
    - name: x
      type: rpc
$RPC_SIDES"
bad bad-type 'type must be rpc or binary' "    - name: x
      type: nfs
      master:
        binaryPath: /opt/x
      node:
        binaryPath: /opt/x"
bad builtin-type 'type must be rpc or binary' "    - name: x
      type: builtin
$RPC_SIDES"
bad missing-master 'master.socketPath is required' "    - name: x
      type: rpc
      node:
        socketPath: /data/cube-shared/run/x.sock"
bad missing-node 'node.socketPath is required' "    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200"
bad missing-binary-path 'node.binaryPath is required' "    - name: x
      type: binary
      master:
        binaryPath: /opt/x
      node:
        socketPath: /data/x.sock"
bad backslash 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: /data/a\form'
bad trailing-backslash 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200\
      node:
        socketPath: /data/a'
bad newline 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: "/data/a\nb"'
bad carriage-return 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: "/data/a\rb"'
bad tab 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: "/data/a\tb"'
bad separator 'may only contain' '    - name: x
      type: rpc
      master:
        socketPath: ctl.svc:9200
      node:
        socketPath: "/data/a;y|rpc|/b"'
bad quote 'may only contain' "    - name: x
      type: rpc
      master:
        socketPath: 'ctl\"svc'
      node:
        socketPath: /data/a"
bad not-a-map 'must be a map' '    - myfs'

LIST="$(values not-a-list <<'YAML'
volumePlugins:
  extra:
    name: x
YAML
)"
expect_fail not-a-list 'volumePlugins.extra must be a list' -f "$LIST"

echo "volume plugins guard: ok"
