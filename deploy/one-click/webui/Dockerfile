# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-webui — CubeSandbox dashboard served by OpenResty.
#
# The build context MUST be the repository root so the Dockerfile can reach
# both web/ (frontend source) and deploy/one-click/webui/ (nginx config). The
# adjacent Dockerfile.dockerignore re-opens that context; it relies on BuildKit
# (the default on Docker 23+, or export DOCKER_BUILDKIT=1), because the legacy
# builder ignores per-Dockerfile ignore files and falls back to the repo-root
# .dockerignore, which filters web/ and deploy/ out.
#
# Usage (run from the repository root):
#   docker build -t cube-webui:v1.0 -f deploy/one-click/webui/Dockerfile .
#   docker run -p 80:80 cube-webui:v1.0
#
# On Linux the default upstream uses host.docker.internal, which Docker Engine
# does not define automatically — map it to the host gateway:
#   docker run -p 80:80 --add-host=host.docker.internal:host-gateway cube-webui:v1.0
#
# To override the CubeAPI upstream at runtime, mount a custom nginx.conf:
#   docker run -p 80:80 -v ./my-nginx.conf:/usr/local/openresty/nginx/conf/nginx.conf:ro cube-webui:v1.0

# Base image ARGs used in FROM must be declared before the first FROM so Docker
# treats them as global build args available to later stages.
ARG OPENRESTY_BASE_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat

# ── Build stage ────────────────────────────────────────────────────────────────
FROM node:22-alpine AS builder

WORKDIR /app

# Copy package manifests first so the (expensive) npm ci layer is cached and
# reused unless the dependency set itself changes.
COPY web/package.json web/package-lock.json ./
RUN npm ci

# Copy the rest of the frontend source — only application code recompiles.
COPY web/ ./

# CUBE_VERSION sets the version shown in the UI. This image is not built by the
# release pipeline (which builds web/dist on the host), so pass it explicitly,
# e.g. `docker build --build-arg CUBE_VERSION=v1.0 -f deploy/one-click/webui/Dockerfile .`.
# When unset, vite.config.ts tries `git describe` (unavailable here — no .git in
# the build context) and ultimately falls back to web/package.json's version.
ARG CUBE_VERSION
ENV CUBE_VERSION=${CUBE_VERSION}

RUN npm run build

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ${OPENRESTY_BASE_IMAGE}

RUN sed -i 's#https\?://dl-cdn.alpinelinux.org#http://mirrors.tencent.com#g' /etc/apk/repositories \
    && apk add --no-cache tzdata wget \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

# Default upstreams baked into the image's nginx.conf — replace at build time.
# Mount a custom nginx.conf at runtime to change these without rebuilding.
#   WEB_UI_UPSTREAM        → cube-api   (the /cubeapi/ backend)
#   SANDBOX_PROXY_UPSTREAM → cube-proxy (the /sandbox/ backend, port 80)
ARG WEB_UI_UPSTREAM=http://host.docker.internal:3000
ARG SANDBOX_PROXY_UPSTREAM=http://host.docker.internal:80

COPY --from=builder /app/dist /usr/share/nginx/html
COPY deploy/one-click/webui/nginx.conf /usr/local/openresty/nginx/conf/nginx.conf

# Escape sed-significant characters (& # \) so an upstream URL passed via
# --build-arg cannot corrupt the sed substitution. nginx.conf carries TWO
# placeholders (cube-api and cube-proxy); both MUST be substituted, otherwise
# OpenResty refuses to start with "invalid URL prefix". The grep guard fails the
# build loudly if any placeholder is left behind.
RUN upstream_escaped=$(printf '%s' "${WEB_UI_UPSTREAM}" | sed -e 's/[&#\\]/\\&/g') \
    && sandbox_upstream_escaped=$(printf '%s' "${SANDBOX_PROXY_UPSTREAM}" | sed -e 's/[&#\\]/\\&/g') \
    && sed -i "s#__WEB_UI_UPSTREAM__#${upstream_escaped}#g" /usr/local/openresty/nginx/conf/nginx.conf \
    && sed -i "s#__SANDBOX_PROXY_UPSTREAM__#${sandbox_upstream_escaped}#g" /usr/local/openresty/nginx/conf/nginx.conf \
    && if grep -Eq '__[A-Z_]+__' /usr/local/openresty/nginx/conf/nginx.conf; then \
           echo "FATAL: unsubstituted placeholder remains in nginx.conf" >&2; exit 1; \
       fi

EXPOSE 80

STOPSIGNAL SIGQUIT

HEALTHCHECK --interval=10s --timeout=3s --retries=6 \
    CMD wget -q -O /dev/null http://127.0.0.1/

ENTRYPOINT ["/usr/local/openresty/bin/openresty", "-g", "daemon off;"]
