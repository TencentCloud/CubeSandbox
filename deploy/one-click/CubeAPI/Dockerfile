# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Package build for cube-api.
#
# The build context is the package's CubeAPI/ directory (which ships the
# prebuilt cube-api binary under bin/), so it can be built straight from an
# extracted one-click release bundle:
#
#   docker build -f CubeAPI/Dockerfile -t cube-api:TAG CubeAPI/
#
# See terraform/tencentcloud/build_images.sh.
#
# The prebuilt binary is dynamically linked against glibc, so the runtime image
# must be glibc-based (ubuntu), not musl-based (alpine). On alpine the kernel
# cannot find the ELF interpreter and exec fails with "no such file or
# directory" even though the binary exists.
FROM ubuntu:24.04

# curl is required by the HEALTHCHECK below; tzdata for Asia/Shanghai local time;
# ca-certificates for outbound TLS.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

# Run as an unprivileged user. The app's default log dir is <binary_dir>/log,
# which a non-root user cannot create under /usr/local/bin, so point LOG_DIR at
# a dedicated writable directory owned by the runtime user.
RUN groupadd -r cube && useradd -r -g cube cube \
    && mkdir -p /var/log/cube-api \
    && chown -R cube:cube /var/log/cube-api
ENV LOG_DIR=/var/log/cube-api

# --chmod folds the executable bit into the COPY so the (large, cache-busting)
# binary lands in a single layer instead of being duplicated by a separate
# `RUN chmod`, which would copy the whole file up into a second layer.
COPY --chmod=0755 bin/cube-api /usr/local/bin/cube-api

USER cube

EXPOSE 3000

# SIGTERM → graceful shutdown (axum::serve().with_graceful_shutdown)
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:3000/health

ENTRYPOINT ["cube-api"]
