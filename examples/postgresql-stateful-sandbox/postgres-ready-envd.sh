#!/bin/sh
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

set -eu

POSTGRES_SOCKET_DIR="${POSTGRES_SOCKET_DIR:-/var/run/postgresql}"
POSTGRES_READY_TIMEOUT="${POSTGRES_READY_TIMEOUT:-60}"

case "${POSTGRES_READY_TIMEOUT}" in
    ''|*[!0-9]*)
        echo "postgres-ready-envd: POSTGRES_READY_TIMEOUT must be a non-negative integer" >&2
        exit 2
        ;;
esac

elapsed=0
until gosu postgres pg_isready -q \
    -h "${POSTGRES_SOCKET_DIR}" -U postgres -d postgres; do
    if [ "${elapsed}" -ge "${POSTGRES_READY_TIMEOUT}" ]; then
        echo "postgres-ready-envd: PostgreSQL was not ready after ${POSTGRES_READY_TIMEOUT}s" >&2
        exit 1
    fi

    sleep 1
    elapsed=$((elapsed + 1))
done

echo "postgres-ready-envd: PostgreSQL is ready; starting envd" >&2
exec /usr/bin/envd "$@"
