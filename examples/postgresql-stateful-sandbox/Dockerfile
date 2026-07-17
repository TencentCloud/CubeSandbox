# syntax=docker/dockerfile:1.7
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

# envd is copied from the pinned CubeSandbox base image, while PostgreSQL
# itself comes from the official PostgreSQL image. The local entrypoint waits
# for PostgreSQL to complete shutdown before the container exits.
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16 AS cube-envd

FROM postgres:16.14-bookworm

ARG DEBIAN_FRONTEND=noninteractive

ENV ENVD_PORT=49983 \
    ENVD_BIN=/usr/local/bin/postgres-ready-envd.sh \
    PGDATA=/var/lib/postgresql/cube-data \
    POSTGRES_SOCKET_DIR=/var/run/postgresql \
    POSTGRES_READY_TIMEOUT=60 \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        tini \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --user-group --shell /bin/bash user

COPY --from=cube-envd /usr/bin/envd /usr/bin/envd

# Keep the database cluster outside the VOLUME inherited from the upstream
# image. Cube snapshots then capture PGDATA and pg_wal in the sandbox rootfs.
# Peer authentication is sufficient because examples execute psql as the
# postgres operating-system user. TCP authentication is rejected explicitly.
RUN chmod +x /usr/bin/envd \
    && install -d -o postgres -g postgres -m 0700 "${PGDATA}" \
    && gosu postgres initdb \
        --pgdata="${PGDATA}" \
        --username=postgres \
        --encoding=UTF8 \
        --locale=C.UTF-8 \
        --data-checksums \
        --auth-local=peer \
        --auth-host=reject \
    && mkdir -p /var/log

# Keep frequently edited runtime scripts after initdb so script-only changes
# do not invalidate the initialized database-cluster layer.
COPY cube-entrypoint.sh start-postgres.sh postgres-ready-envd.sh /usr/local/bin/

RUN chmod +x \
        /usr/local/bin/cube-entrypoint.sh \
        /usr/local/bin/start-postgres.sh \
        /usr/local/bin/postgres-ready-envd.sh

LABEL org.opencontainers.image.source="https://github.com/TencentCloud/CubeSandbox" \
      org.opencontainers.image.title="cubesandbox-postgresql-stateful" \
      org.opencontainers.image.description="Stateful PostgreSQL 16.14 CubeSandbox template" \
      org.opencontainers.image.licenses="Apache-2.0"

EXPOSE 49983

HEALTHCHECK --interval=2s --timeout=2s --start-period=10s --retries=30 \
    CMD curl -fsS -o /dev/null "http://127.0.0.1:${ENVD_PORT}/health" \
        && gosu postgres pg_isready -q --timeout=1 \
            -h "${POSTGRES_SOCKET_DIR}" -U postgres -d postgres

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/cube-entrypoint.sh"]
CMD ["/usr/local/bin/start-postgres.sh"]
