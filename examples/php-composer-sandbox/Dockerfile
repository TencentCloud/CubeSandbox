# syntax=docker/dockerfile:1.7
#
# A Cube-ready PHP + Composer application image.  cubesandbox-base supplies
# envd and cube-entrypoint, so Cube can use :49983/health as its readiness
# probe while this example application listens on :8080.
#
# Build:
#   docker build --platform linux/amd64 -t php-composer-cube:latest .

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# Leave empty to use Ubuntu's default archive. Set to a reachable Ubuntu mirror
# (without a trailing slash) only when the build environment cannot reach it.
ARG APT_MIRROR=

RUN set -eux; \
    if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR}|g" /etc/apt/sources.list; \
    fi; \
    for attempt in 1 2 3; do \
        apt-get update \
        && apt-get install -y --no-install-recommends \
            composer \
            php-cli \
            php-mbstring \
            php-xml \
            unzip \
        && break; \
        if [ "${attempt}" = 3 ]; then exit 1; fi; \
        rm -rf /var/lib/apt/lists/*; \
        sleep 5; \
    done; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

COPY app/composer.json ./composer.json
COPY app/public ./public
COPY app/router.php ./router.php

# The sample deliberately has no third-party runtime dependency.  Still run
# Composer during the build so this image proves Composer is ready for a real
# project and future dependencies can be resolved reproducibly from this file.
RUN composer validate --no-check-lock --no-interaction \
    && mkdir -p /workspace \
    && chown -R user:user /workspace

EXPOSE 49983 8080

# cube-entrypoint must start envd as root: envd creates /run/e2b and cgroup
# helpers.  Drop only the application process to the base image's unprivileged
# `user` account after envd is running.
CMD ["sudo", "-u", "user", "--", "php", "-S", "0.0.0.0:8080", "-t", "public", "router.php"]
