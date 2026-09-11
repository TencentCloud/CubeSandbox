#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail
ONE_CLICK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ONE_CLICK_DIR}/lib/common.sh"
task_tmp="$(mktemp -d)"
trap 'rm -rf "$task_tmp"' EXIT
cp "${ONE_CLICK_DIR}/../../Cubelet/config/config.toml" "$task_tmp/config.toml"
# Spaces and quotes must survive TOML rendering and environment persistence.
image_dir='/data/template cache/"images"'
write_cubelet_artifact_paths "$task_tmp/config.toml" "$image_dir" '/opt/kernel/vmlinux'
cp "$task_tmp/config.toml" "$task_tmp/once.toml"
write_cubelet_artifact_paths "$task_tmp/config.toml" "$image_dir" '/opt/kernel/vmlinux'
cmp "$task_tmp/once.toml" "$task_tmp/config.toml"
python3 - "$task_tmp/config.toml" "$image_dir" <<'PY'
import json, sys
from pathlib import Path
text = Path(sys.argv[1]).read_text()
body = text.split('[plugins."io.cubelet.internal.v1.images"]',1)[1].split('[',1)[0]
values = dict(line.strip().split(' = ',1) for line in body.splitlines() if ' = ' in line)
assert json.loads(values['image_base_path']) == sys.argv[2]
assert json.loads(values['shared_kernel_path']) == '/opt/kernel/vmlinux'
assert 'runtime_type = "io.containerd.runc.v2"' in text
PY
if (write_cubelet_artifact_paths "$task_tmp/config.toml" relative /kernel) >/dev/null 2>&1; then
 echo 'FAIL: relative path accepted' >&2; exit 1
fi
cmp "$task_tmp/once.toml" "$task_tmp/config.toml"
# Existing env merge adopts a new key, then preserves a customized runtime value.
printf 'ONE_CLICK_OS_IMAGE_DIR=/data/cubelet/cubebox_os_image\n' > "$task_tmp/new.env"
printf '# old version\n' > "$task_tmp/old.env"
merge_env_three_way "$task_tmp/new.env" "$task_tmp/old.env" "$task_tmp/old.env" '' "$task_tmp/merged.env" "$task_tmp/diff"
grep -qx 'ONE_CLICK_OS_IMAGE_DIR=/data/cubelet/cubebox_os_image' "$task_tmp/merged.env"
printf 'ONE_CLICK_OS_IMAGE_DIR=/custom/images\n' > "$task_tmp/old.env"
merge_env_three_way "$task_tmp/new.env" "$task_tmp/old.env" "$task_tmp/new.env" '' "$task_tmp/merged.env" "$task_tmp/diff"
grep -qx 'ONE_CLICK_OS_IMAGE_DIR=/custom/images' "$task_tmp/merged.env"
echo 'PASS: artifact path rendering and upgrade env merge'

# Persist the effective process-only value, then recover it on the next upgrade.
: > "$task_tmp/runtime.env"
upsert_env_kv "$task_tmp/runtime.env" ONE_CLICK_OS_IMAGE_DIR "$image_dir"
merge_env_three_way "$task_tmp/new.env" "$task_tmp/runtime.env" "$task_tmp/new.env" '' "$task_tmp/merged.env" "$task_tmp/diff"
reloaded="$(source "$task_tmp/merged.env"; printf '%s' "$ONE_CLICK_OS_IMAGE_DIR")"
[[ "$reloaded" == "$image_dir" ]]
grep -Fq 'upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_OS_IMAGE_DIR" "${ONE_CLICK_OS_IMAGE_DIR}"' "$ONE_CLICK_DIR/install.sh"
echo 'PASS: effective artifact directory survives persistence and upgrade'

# Execute install.sh's actual environment-loading prefix, mocking host preflight
# only. This catches overrides lost before rendering/persistence is reached.
fixture="$task_tmp/installer"
mkdir -p "$fixture/lib"
python3 - "$ONE_CLICK_DIR/install.sh" "$fixture/install-env.sh" <<'PY'
import sys
from pathlib import Path
source = Path(sys.argv[1]).read_text()
# Redirect the fixed installation root into the test directory.
source = source.replace('INSTALL_PREFIX="${CUBE_SANDBOX_INSTALL_ROOT}"', 'INSTALL_PREFIX="${TEST_INSTALL_ROOT}"', 1)
marker = '\ninit_external_dep_defaults\n'
assert source.count(marker) == 1
Path(sys.argv[2]).write_text(source.split(marker, 1)[0] + '''
ONE_CLICK_OS_IMAGE_DIR="${ONE_CLICK_OS_IMAGE_DIR:-/data/cubelet/cubebox_os_image}"
write_cubelet_artifact_paths "$TEST_CONFIG" "$ONE_CLICK_OS_IMAGE_DIR" /opt/kernel/vmlinux
upsert_env_kv "$TEST_RUNTIME" ONE_CLICK_OS_IMAGE_DIR "$ONE_CLICK_OS_IMAGE_DIR"
''')
PY
printf 'source %q\n' "$ONE_CLICK_DIR/lib/common.sh" > "$fixture/lib/common.sh"
cat >> "$fixture/lib/common.sh" <<'SH'
require_root() { :; }
preflight_upgrade() { :; }
resolve_install_mode() { printf '%s\n' "$TEST_MODE"; }
SH
cp "$task_tmp/new.env" "$fixture/env.example"

check_install_env() (
  local name="$1" mode="$2" process_value="$3" dotenv_value="$4" old_value="$5" expected="$6"
  local case_dir="$task_tmp/$name"
  mkdir -p "$case_dir/installed"
  printf '# previous release\n' > "$case_dir/installed/env.example"
  : > "$case_dir/installed/.one-click.env"
  : > "$case_dir/input.env"
  if [[ "$old_value" != UNSET ]]; then
    upsert_env_kv "$case_dir/installed/.one-click.env" ONE_CLICK_OS_IMAGE_DIR "$old_value"
  fi
  if [[ "$dotenv_value" != UNSET ]]; then
    upsert_env_kv "$case_dir/input.env" ONE_CLICK_OS_IMAGE_DIR "$dotenv_value"
  fi
  unset ONE_CLICK_OS_IMAGE_DIR
  if [[ "$process_value" != UNSET ]]; then
    export ONE_CLICK_OS_IMAGE_DIR="$process_value"
  fi
  cp "$ONE_CLICK_DIR/../../Cubelet/config/config.toml" "$case_dir/config.toml"
  env TEST_MODE="$mode" TEST_CONFIG="$case_dir/config.toml" TEST_RUNTIME="$case_dir/result.env" \
    TEST_INSTALL_ROOT="$case_dir/installed" ONE_CLICK_ENV_FILE="$case_dir/input.env" \
    bash "$fixture/install-env.sh" > "$case_dir/install.log" 2>&1 || {
      cat "$case_dir/install.log" >&2; exit 1;
    }
  unset ONE_CLICK_OS_IMAGE_DIR
  source "$case_dir/result.env"
  [[ "$ONE_CLICK_OS_IMAGE_DIR" == "$expected" ]] || {
    printf 'FAIL %s: expected %s, got %s\n' "$name" "$expected" "$ONE_CLICK_OS_IMAGE_DIR" >&2
    exit 1
  }
  python3 - "$case_dir/config.toml" "$expected" <<'PY'
import json, sys
from pathlib import Path
body = Path(sys.argv[1]).read_text().split('[plugins."io.cubelet.internal.v1.images"]', 1)[1].split('[', 1)[0]
values = dict(line.strip().split(' = ', 1) for line in body.splitlines() if ' = ' in line)
assert json.loads(values['image_base_path']) == sys.argv[2]
PY
  printf 'PASS: installer environment order: %s\n' "$name"
)
check_install_env fresh-process install "$image_dir" UNSET UNSET "$image_dir"
check_install_env upgrade-process upgrade "$image_dir" UNSET UNSET "$image_dir"
check_install_env upgrade-replace-custom upgrade /new/images UNSET /old/images /new/images
check_install_env upgrade-dotenv-priority upgrade /process/images /dotenv/images /old/images /dotenv/images
check_install_env upgrade-dotenv-default upgrade /process/images /data/cubelet/cubebox_os_image /old/images /data/cubelet/cubebox_os_image
check_install_env upgrade-new-default upgrade UNSET UNSET UNSET /data/cubelet/cubebox_os_image
check_install_env upgrade-preserve-custom upgrade UNSET UNSET /old/images /old/images
check_install_env upgrade-copied-example upgrade UNSET /data/cubelet/cubebox_os_image /old/images /old/images
check_install_env upgrade-dotenv-custom upgrade UNSET /dotenv/images /old/images /dotenv/images
