# syntax=docker/dockerfile:1.7
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

ARG ENVD_REF=2026.16
ARG GO_VERSION=1.25.4
ARG CUBEVZ_TEMPLATE_IMAGE=alpine:3.22

FROM --platform=$TARGETPLATFORM ${CUBEVZ_TEMPLATE_IMAGE} AS template-image

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS envd-builder

ARG ENVD_REF
ARG TARGETARCH
ARG ENVD_UPSTREAM_REPO=https://github.com/e2b-dev/infra.git

RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone --depth 1 --branch "${ENVD_REF}" "${ENVD_UPSTREAM_REPO}" infra
WORKDIR /src/infra/packages/envd
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    commit_sha="$(git -C /src/infra rev-parse --short HEAD)" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
      go build -a -ldflags "-X=main.commitSHA=${commit_sha} -s -w" \
      -o /out/envd . && \
    chmod 0755 /out/envd && \
    printf '%s\n' "${ENVD_REF}" > /out/envd-ref

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS egress-builder

ARG TARGETARCH
WORKDIR /src
COPY cube-vz-egress.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
      go build -trimpath -ldflags "-s -w" -o /out/cube-vz-egress cube-vz-egress.go

FROM --platform=$TARGETPLATFORM alpine:3.22 AS builder

ARG ALPINE_VERSION=3.22

RUN apk add --no-cache build-base e2fsprogs grep linux-headers openssl

RUN mkdir -p /rootfs && \
    apk \
      --root /rootfs \
      --initdb \
      --no-cache \
      --keys-dir /etc/apk/keys \
      --repository "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main" \
      --repository "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/community" \
      add alpine-base bash ca-certificates coreutils curl dnsmasq iproute2 ipset iptables linux-virt python3 sysbench

# Overlay the requested ARM64 OCI image onto the serviceable Alpine guest.
# COPY is additive, so the kernel, envd dependencies and recovery shell remain
# available even for distroless source images. CubeVZ services are copied again
# below so a source image cannot replace the host/guest control plane.
COPY --from=template-image / /rootfs/

COPY --from=envd-builder /out/envd /rootfs/usr/bin/envd
COPY --from=envd-builder /out/envd-ref /rootfs/etc/cubesandbox-envd-ref
COPY --from=egress-builder /out/cube-vz-egress /rootfs/usr/local/sbin/cube-vz-egress

COPY guest-init.sh /rootfs/usr/local/sbin/cube-vz-bench-init
COPY lifecycle-init.sh /rootfs/usr/local/sbin/cube-vz-lifecycle-init
COPY cube-vz-exec.py /rootfs/usr/local/sbin/cube-vz-exec
COPY cube-vz-template-entrypoint.py /rootfs/usr/local/sbin/cube-vz-template-entrypoint
COPY cube-vz-agent.c /tmp/cube-vz-agent.c

ARG ROOTFS_SIZE_MIB=768
ARG CUBEVZ_TEMPLATE_IMAGE=alpine:3.22
ARG CUBEVZ_TEMPLATE_CONFIG_B64=

RUN cc -static -O2 -Wall -Wextra -Werror -pthread \
      -o /rootfs/usr/local/sbin/cube-vz-agent /tmp/cube-vz-agent.c && \
    chmod 0755 /rootfs/usr/local/sbin/cube-vz-bench-init \
      /rootfs/usr/local/sbin/cube-vz-lifecycle-init \
      /rootfs/usr/local/sbin/cube-vz-exec \
      /rootfs/usr/local/sbin/cube-vz-template-entrypoint \
      /rootfs/usr/local/sbin/cube-vz-agent && \
    mkdir -p /rootfs/dev /rootfs/home/user /rootfs/proc /rootfs/sys /rootfs/run \
      /rootfs/tmp /rootfs/var/lib/cube-vz-bench /rootfs/var/log \
      /rootfs/etc/cube-vz /rootfs/usr/local/share/ca-certificates /out && \
    ln -sf /bin/nice /rootfs/usr/bin/nice && \
    printf 'user:x:1000:1000:Sandbox User:/home/user:/bin/bash\n' >> /rootfs/etc/passwd && \
    printf 'user:x:1000:\n' >> /rootfs/etc/group && \
    printf 'cube-proxy:x:8049:8049:CubeVZ Egress:/nonexistent:/sbin/nologin\n' >> /rootfs/etc/passwd && \
    printf 'cube-proxy:x:8049:\n' >> /rootfs/etc/group && \
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
      -sha256 -days 3650 -nodes -batch -subj '/CN=CubeVZ Local Egress CA' \
      -keyout /rootfs/etc/cube-vz/egress-ca.key \
      -out /rootfs/etc/cube-vz/egress-ca.crt && \
    cp /rootfs/etc/cube-vz/egress-ca.crt \
      /rootfs/usr/local/share/ca-certificates/cube-vz-egress.crt && \
    cat /rootfs/etc/cube-vz/egress-ca.crt >> /rootfs/etc/ssl/certs/ca-certificates.crt && \
    touch /rootfs/var/log/cube-vz-egress.jsonl && \
    chown 8049:8049 /rootfs/etc/cube-vz/egress-ca.key \
      /rootfs/var/log/cube-vz-egress.jsonl && \
    chmod 0440 /rootfs/etc/cube-vz/egress-ca.key && \
    if [ -n "${CUBEVZ_TEMPLATE_CONFIG_B64}" ]; then \
      printf '%s' "${CUBEVZ_TEMPLATE_CONFIG_B64}" | base64 -d \
        > /rootfs/etc/cube-vz/template-runtime.json; \
    fi && \
    chown -R 1000:1000 /rootfs/home/user && \
    chmod 1777 /rootfs/tmp && \
    kernel_offset="$(grep -abo -m1 "$(printf '\037\213\010')" \
      /rootfs/boot/vmlinuz-virt | cut -d: -f1)" && \
    test -n "${kernel_offset}" && \
    tail -c "+$((kernel_offset + 1))" /rootfs/boot/vmlinuz-virt | \
      gzip -dc > /out/kernel && \
    cp /rootfs/boot/initramfs-virt /out/initrd && \
    truncate -s "${ROOTFS_SIZE_MIB}M" /out/rootfs.raw && \
    mkfs.ext4 -q -F -b 4096 -d /rootfs /out/rootfs.raw && \
    sha256sum /out/kernel /out/initrd /out/rootfs.raw > /out/SHA256SUMS && \
    printf 'alpine=%s\nrootfs_size_mib=%s\nsource_image=%s\n' \
      "$(cat /rootfs/etc/alpine-release)" "${ROOTFS_SIZE_MIB}" \
      "${CUBEVZ_TEMPLATE_IMAGE}" > /out/build-info.txt

FROM scratch
COPY --from=builder /out/ /
