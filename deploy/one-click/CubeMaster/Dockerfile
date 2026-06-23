# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Package build for cubemaster.
#
# The build context is the package's CubeMaster/ directory (which ships the
# prebuilt cubemaster binary under bin/), so it can be built straight from an
# extracted one-click release bundle:
#
#   docker build -f CubeMaster/Dockerfile -t cubemaster:TAG CubeMaster/
#
# See terraform/tencentcloud/build_images.sh.
#
# The prebuilt cubemaster binary is a default `go build` (cgo-enabled) artifact
# dynamically linked against glibc, so the runtime image must be glibc-based
# (ubuntu), not musl-based (alpine): on alpine the kernel cannot find the ELF
# interpreter and exec fails with "no such file or directory" even though the
# binary exists.
FROM ubuntu:24.04

# Single RUN layer with --no-install-recommends and an apt-cache cleanup so the
# image stays as small as practical given the bundled toolset:
#   - skopeo, umoci: runtime deps; cubemaster shells out to them to pull and
#     unpack OCI template images (see pkg/templatecenter/image).
#   - curl: required by the HEALTHCHECK below.
#   - ca-certificates, tzdata: outbound TLS and Asia/Shanghai local time
#     (ca-certificates is refreshed via update-ca-certificates below).
#   - everything else: on-node debugging/networking tools kept intentionally;
#     a deliberate attack-surface/CVE tradeoff for on-node debuggability.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        bash-completion \
        bpftrace \
        busybox \
        ca-certificates \
        curl \
        dnsutils \
        ethtool \
        file \
        gawk \
        iperf \
        iperf3 \
        iproute2 \
        iputils-ping \
        netcat-openbsd \
        netperf \
        net-tools \
        sysstat \
        tcpdump \
        telnet \
        tzdata \
        vim \
        wget \
        skopeo \
        umoci \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && update-ca-certificates

# --chmod folds the executable bit into the COPY so the (large, cache-busting)
# binary lands in a single layer instead of being duplicated by a separate
# `RUN chmod`, which would copy the whole file up into a second layer. The COPY
# also creates the parent directory, so no separate `mkdir` is needed.
COPY --chmod=0755 bin/cubemaster /usr/local/services/cubemaster/cubemaster

# Fail the build early if the binary cannot exec in this image (wrong arch, or a
# missing/incompatible ELF interpreter / shared library) instead of only
# discovering it at `docker run`. `--version` prints the version and exits
# before any config or database initialization, so it needs no runtime deps.
RUN ["/usr/local/services/cubemaster/cubemaster", "--version"]

# No USER directive: cubemaster intentionally runs as root. It shells out to
# `umoci unpack` to materialize OCI template images, and only as root (without
# --rootless) does umoci preserve each image's original file uid/gid; a non-root
# unpack squashes ownership and crash-loops images that expect non-root files
# (see pkg/templatecenter/image/export.go). Constrain it at deploy time (drop
# capabilities, seccomp/AppArmor, read-only rootfs) rather than here.

WORKDIR /usr/local/services/cubemaster/

EXPOSE 8089

# SIGTERM -> graceful shutdown (stops the scheduler and drains in-flight
# requests/tasks before exit). This is Docker's default stop signal; set it
# explicitly to make the contract visible and consistent with CubeAPI/Dockerfile.
STOPSIGNAL SIGTERM

# The HTTP listener runs in a background goroutine whose bind error is only
# logged while the process keeps waiting on signals, so the container can stay
# "up" while not actually serving. Probe the notify health route (default
# http_port 8089) to catch that. curl is installed above. --start-period covers
# a slow first boot: the listener only comes up after DB migrations, which can
# wait on a ~60s advisory lock before the container should be deemed unhealthy.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=60s \
    CMD curl -fsS http://127.0.0.1:8089/notify/health

ENTRYPOINT ["/usr/local/services/cubemaster/cubemaster"]
