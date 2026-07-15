# syntax=docker/dockerfile:1.7
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

ARG BASE_IMAGE
FROM ${BASE_IMAGE} AS runtime

FROM --platform=$TARGETPLATFORM alpine:3.22 AS packager

ARG ROOTFS_SIZE_MIB=2048

RUN apk add --no-cache build-base busybox-static e2fsprogs grep linux-headers linux-virt

COPY --from=runtime / /rootfs/
COPY CubeVZ/Guest/init.sh /rootfs/usr/local/sbin/cube-vz-init
COPY CubeVZ/Guest/udhcpc.sh /rootfs/usr/local/sbin/cube-vz-udhcpc
COPY CubeVZ/Benchmark/cube-vz-agent.c /tmp/cube-vz-agent.c
COPY CubeVZ/Benchmark/cube-vz-relay.c /tmp/cube-vz-relay.c

RUN cc -static -O2 -Wall -Wextra -Werror \
      -o /rootfs/usr/local/sbin/cube-vz-agent /tmp/cube-vz-agent.c && \
    cc -static -O2 -Wall -Wextra -Werror -pthread \
      -o /rootfs/usr/local/sbin/cube-vz-relay /tmp/cube-vz-relay.c && \
    cp /bin/busybox.static /rootfs/usr/local/sbin/busybox && \
    cp -a /lib/modules /rootfs/lib/ && \
    chmod 0755 /rootfs/usr/local/sbin/busybox /rootfs/usr/local/sbin/cube-vz-* && \
    mkdir -p /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/run /rootfs/tmp /out && \
    chmod 1777 /rootfs/tmp && \
    kernel_offset="$(grep -abo -m1 "$(printf '\037\213\010')" \
      /boot/vmlinuz-virt | cut -d: -f1)" && \
    test -n "${kernel_offset}" && \
    tail -c "+$((kernel_offset + 1))" /boot/vmlinuz-virt | gzip -dc > /out/kernel && \
    cp /boot/initramfs-virt /out/initrd && \
    truncate -s "${ROOTFS_SIZE_MIB}M" /out/rootfs.raw && \
    mkfs.ext4 -q -F -b 4096 -d /rootfs /out/rootfs.raw && \
    sha256sum /out/kernel /out/initrd /out/rootfs.raw > /out/SHA256SUMS && \
    printf 'envd_ref=%s\nrootfs_size_mib=%s\n' \
      "$(cat /rootfs/etc/cubesandbox-envd-ref)" "${ROOTFS_SIZE_MIB}" > /out/build-info.txt

FROM scratch
COPY --from=packager /out/ /
