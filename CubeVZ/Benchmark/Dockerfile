# syntax=docker/dockerfile:1.7
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

FROM --platform=$TARGETPLATFORM alpine:3.22 AS builder

ARG ALPINE_VERSION=3.22
ARG ROOTFS_SIZE_MIB=768

RUN apk add --no-cache build-base e2fsprogs grep linux-headers

RUN mkdir -p /rootfs && \
    apk \
      --root /rootfs \
      --initdb \
      --no-cache \
      --keys-dir /etc/apk/keys \
      --repository "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main" \
      --repository "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/community" \
      add alpine-base linux-virt sysbench

COPY guest-init.sh /rootfs/usr/local/sbin/cube-vz-bench-init
COPY lifecycle-init.sh /rootfs/usr/local/sbin/cube-vz-lifecycle-init
COPY cube-vz-agent.c /tmp/cube-vz-agent.c

RUN cc -static -O2 -Wall -Wextra -Werror \
      -o /rootfs/usr/local/sbin/cube-vz-agent /tmp/cube-vz-agent.c && \
    chmod 0755 /rootfs/usr/local/sbin/cube-vz-bench-init \
      /rootfs/usr/local/sbin/cube-vz-lifecycle-init \
      /rootfs/usr/local/sbin/cube-vz-agent && \
    mkdir -p /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/run /rootfs/tmp \
      /rootfs/var/lib/cube-vz-bench /out && \
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
    printf 'alpine=%s\nrootfs_size_mib=%s\n' \
      "$(cat /rootfs/etc/alpine-release)" "${ROOTFS_SIZE_MIB}" > /out/build-info.txt

FROM scratch
COPY --from=builder /out/ /
