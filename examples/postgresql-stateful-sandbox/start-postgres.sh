#!/bin/sh
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

set -eu

PGDATA="${PGDATA:-/var/lib/postgresql/cube-data}"
POSTGRES_SOCKET_DIR="${POSTGRES_SOCKET_DIR:-/var/run/postgresql}"

if [ ! -s "${PGDATA}/PG_VERSION" ]; then
    echo "start-postgres: initialized cluster not found at ${PGDATA}" >&2
    exit 1
fi

# /run is normally a tmpfs, so its contents and permissions must be restored
# on every container boot. The socket is only accessible to postgres' group.
install -d -o postgres -g postgres -m 0770 "${POSTGRES_SOCKET_DIR}"

exec gosu postgres postgres \
    -D "${PGDATA}" \
    -c "listen_addresses=" \
    -c "unix_socket_directories=${POSTGRES_SOCKET_DIR}" \
    -c "unix_socket_permissions=0770" \
    -c "max_connections=20"
