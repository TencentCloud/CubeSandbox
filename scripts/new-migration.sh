#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Create a new CubeMaster MySQL migration with a UTC timestamp version prefix.
#
# Why timestamps (not sequential numbers): goose tracks migrations only by the
# integer filename prefix. Sequential numbers get reused across rebases, which
# silently skips a migration. A UTC timestamp is unique and never needs renaming
# on rebase. See migrations/mysql/README.md for the full rationale.
#
# Usage:
#   scripts/new-migration.sh <description>
#
# Example:
#   scripts/new-migration.sh add_foo_column
#   -> CubeMaster/pkg/base/dao/migrate/migrations/mysql/20260622143000_add_foo_column.sql

set -euo pipefail

if [[ $# -ne 1 || -z "${1:-}" ]]; then
  echo "usage: $0 <description>   (e.g. add_foo_column)" >&2
  exit 2
fi

description="$1"

# Enforce the same shape the CI naming check requires: lowercase letters,
# digits and underscores only.
if [[ ! "${description}" =~ ^[a-z0-9_]+$ ]]; then
  echo "error: description must match ^[a-z0-9_]+$ (got: ${description})" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
mysql_dir="${repo_root}/CubeMaster/pkg/base/dao/migrate/migrations/mysql"

if [[ ! -d "${mysql_dir}" ]]; then
  echo "error: migrations dir not found: ${mysql_dir}" >&2
  exit 1
fi

version="$(date -u +%Y%m%d%H%M%S)"
file="${mysql_dir}/${version}_${description}.sql"

if [[ -e "${file}" ]]; then
  echo "error: ${file} already exists (run again in 1s for a new timestamp)" >&2
  exit 1
fi

lock_name="cubemaster_migration_${version}_${description}"
year="$(date -u +%Y)"

cat > "${file}" <<EOF
-- Copyright (c) ${year} Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- TODO: describe what this migration does and why it is safe to apply
-- out-of-order (it must be, because goose runs with WithAllowOutofOrder).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('${lock_name}', 60);

-- TODO: use the idempotent helpers from the baseline, e.g.
-- CALL cubemaster_add_column_if_missing('t_cube_foo', 'bar', "varchar(64) NOT NULL DEFAULT ''");

SELECT RELEASE_LOCK('${lock_name}');

-- +goose Down

CALL cubemaster_acquire_migration_lock('${lock_name}', 60);

-- TODO: reverse the Up changes idempotently, e.g.
-- CALL cubemaster_drop_column_if_exists('t_cube_foo', 'bar');

SELECT RELEASE_LOCK('${lock_name}');
EOF

echo "created ${file}"
