# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Package build for cube-ops.
#
# The build context is the package's CubeOps/ directory (which ships the
# prebuilt cubeops binary under bin/), so it can be built straight from an
# extracted one-click release bundle:
#
#   docker build -f CubeOps/Dockerfile -t cube-ops:TAG CubeOps/
#
# See terraform/tencentcloud/build_images.sh.
FROM ubuntu:24.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

COPY --chmod=0755 bin/cubeops /usr/local/bin/cubeops

EXPOSE 3010

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:3010/health

ENTRYPOINT ["cubeops"]
