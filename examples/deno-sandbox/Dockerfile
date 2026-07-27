# syntax=docker/dockerfile:1.7
#
# Deno 2 + TypeScript runtime template for CubeSandbox.
#
# Build:
#   docker build -t deno-sandbox:2.8.1 examples/deno-sandbox

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG DENO_VERSION=2.8.1
ARG TARGETARCH

# Deno is distributed as one self-contained binary. Pin the release, select the
# official x64/arm64 asset, verify its matching checksum, and remove all build
# utilities from the final layer.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl util-linux unzip \
    && arch="${TARGETARCH:-$(uname -m)}" \
    && case "${arch}" in \
         amd64|x86_64) deno_arch="x86_64" ;; \
         arm64|aarch64) deno_arch="aarch64" ;; \
         *) echo "Unsupported architecture: ${arch}" >&2; exit 1 ;; \
       esac \
    && archive="deno-${deno_arch}-unknown-linux-gnu.zip" \
    && release_url="https://github.com/denoland/deno/releases/download/v${DENO_VERSION}" \
    && curl -fsSL "${release_url}/${archive}" -o "/tmp/${archive}" \
    && curl -fsSL "${release_url}/${archive}.sha256sum" -o "/tmp/${archive}.sha256sum" \
    && (cd /tmp && sha256sum -c "${archive}.sha256sum") \
    && unzip -q "/tmp/${archive}" -d /tmp/deno-bin \
    && install -m 0755 /tmp/deno-bin/deno /usr/local/bin/deno \
    && deno --version \
    && rm -rf /tmp/deno-bin "/tmp/${archive}" "/tmp/${archive}.sha256sum" \
    && apt-get purge -y curl unzip \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

ENV DENO_DIR=/home/user/.cache/deno \
    DENO_NO_UPDATE_CHECK=1

RUN mkdir -p /workspace/deno-app/data "${DENO_DIR}" \
    && chown -R 1000:1000 /workspace/deno-app /home/user/.cache

WORKDIR /workspace/deno-app

# Copy dependency metadata first so source-only changes retain the dependency
# cache layer. deno.lock makes the JSR dependency graph reproducible.
COPY --chown=1000:1000 deno.json deno.lock ./
COPY --chown=1000:1000 main.ts main_test.ts ./

RUN runuser --user user -- deno cache --frozen main.ts main_test.ts \
    && runuser --user user -- deno task verify

# 49983 is the inherited envd endpoint used by Cube's readiness probe. The
# Deno demo service starts on demand on port 8000 from the host-side scripts.
EXPOSE 8000 49983
