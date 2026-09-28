# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Package build for cube-templatecenter.
#
# The build context is the package's CubeTemplateCenter/ directory (which
# ships the prebuilt templatecenter binary under bin/), so it can be built
# straight from an extracted one-click release bundle:
#
#   docker build -f CubeTemplateCenter/Dockerfile -t cube-templatecenter:TAG CubeTemplateCenter/
#
# See terraform/tencentcloud/build_images.sh. The runtime toolset mirrors the
# repo-root release image (CubeTemplateCenter/docker/Dockerfile).
FROM ubuntu:24.04

# skopeo + umoci are MANDATORY, not diagnostic conveniences: this process is
# the one that actually pulls the source image and unpacks its layers before
# mkfs. e2fsprogs provides mkfs.ext4, and mount/losetup (util-linux) back the
# loop-device path. A missing tool surfaces late, as a failed build reported
# over the status callback, so they are pinned in the image rather than
# assumed. curl backs the HEALTHCHECK below.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        e2fsprogs \
        file \
        gawk \
        iproute2 \
        iputils-ping \
        netcat-openbsd \
        skopeo \
        tzdata \
        umoci \
        util-linux \
        xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && update-ca-certificates

# Layout matches the one-click install prefix:
#   /usr/local/services/cubetoolbox/CubeTemplateCenter/{bin,conf.yaml}
#
# --chmod folds the executable bit into the COPY so the (large, cache-busting)
# binary lands in a single layer instead of being duplicated by a separate
# `RUN chmod`.
COPY --chmod=0755 bin/templatecenter \
    /usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter

# Fail the build early if the binary cannot exec in this image (wrong arch,
# or a missing/incompatible ELF interpreter) instead of only discovering it
# at `docker run`.
RUN ["/usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter", "--version"]

# No USER directive: templatecenter intentionally runs as root, for the same
# reason cubemaster does -- `umoci unpack` only preserves each image's
# original file uid/gid when run as root (see
# CubeMaster/pkg/templatecenter/image/export.go).

WORKDIR /usr/local/services/cubetoolbox/CubeTemplateCenter/

# Config is read from an environment variable, NOT from a -conf flag (the
# process reuses CubeMaster's config loader). Deployments mount conf.yaml
# here.
ENV CUBE_TEMPLATE_CENTER_CONFIG_PATH=/usr/local/services/cubetoolbox/CubeTemplateCenter/conf.yaml

EXPOSE 8090

STOPSIGNAL SIGTERM

# start-period is generous because the first boot waits on CubeMaster's
# schema migration when both start together.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=60s \
    CMD curl -fsS http://127.0.0.1:8090/health

ENTRYPOINT ["/usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter"]
