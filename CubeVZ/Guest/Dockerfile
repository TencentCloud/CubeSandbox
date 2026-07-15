# syntax=docker/dockerfile:1.7
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

ARG BASE_IMAGE
FROM ${BASE_IMAGE} AS runtime

FROM --platform=$TARGETPLATFORM alpine:3.22 AS kernel-builder

ARG KERNEL_VERSION=6.12.95
ARG KERNEL_SHA256=a9e8c51fcb1e695d1d35dde5886cba579cb6f29c9646c5889f39d63841d4b9f6

RUN apk add --no-cache \
      bc \
      bash \
      binutils \
      bison \
      build-base \
      flex \
      openssl-dev \
      perl \
      xz

WORKDIR /kernel
RUN wget -q "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" && \
    echo "${KERNEL_SHA256}  linux-${KERNEL_VERSION}.tar.xz" | sha256sum -c - && \
    tar -xf "linux-${KERNEL_VERSION}.tar.xz" --strip-components=1 && \
    rm "linux-${KERNEL_VERSION}.tar.xz"

# Start from the repository's proven ARM64 microVM configuration. It already
# builds ext4 and the required virtio block, network, console, entropy, and
# vsock transports into the kernel, so CubeVZ can boot the root disk directly
# without a generic distribution initramfs or loadable modules.
COPY hypervisor/resources/linux-config-aarch64 /kernel/.config
RUN make olddefconfig && \
    scripts/config \
      --disable BLK_DEV_INITRD \
      --disable MODULES \
      --disable DEBUG_INFO \
      --disable FTRACE \
      --disable KPROBES \
      --disable PROFILING \
      --disable IKCONFIG \
      --disable LOCALVERSION_AUTO \
      --disable TASKSTATS \
      --disable KVM \
      --disable BINFMT_MISC \
      --disable XFRM \
      --disable NET_KEY \
      --disable BLK_DEV_NULL_BLK \
      --disable ZRAM \
      --disable BLK_DEV_LOOP \
      --disable BLK_DEV_RAM \
      --disable MD \
      --disable BLK_DEV_DM \
      --disable TUN \
      --disable INPUT \
      --disable PPS \
      --disable HID \
      --disable VFIO \
      --disable VIRTIO_BALLOON \
      --disable FS_ENCRYPTION \
      --disable FUSE_FS \
      --disable OVERLAY_FS \
      --disable SQUASHFS \
      --set-str LOCALVERSION "-cube-vz" && \
    make olddefconfig && \
    make -j"$(nproc)" Image && \
    install -Dm0644 arch/arm64/boot/Image /out/kernel

FROM --platform=$TARGETPLATFORM alpine:3.22 AS packager

ARG KERNEL_VERSION=6.12.95
ARG ROOTFS_SIZE_MIB=2048

RUN apk add --no-cache build-base busybox-static e2fsprogs linux-headers

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
    chmod 0755 /rootfs/usr/local/sbin/busybox /rootfs/usr/local/sbin/cube-vz-* && \
    mkdir -p /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/run /rootfs/tmp /out && \
    chmod 1777 /rootfs/tmp && \
    truncate -s "${ROOTFS_SIZE_MIB}M" /out/rootfs.raw && \
    mkfs.ext4 -q -F -b 4096 -d /rootfs /out/rootfs.raw && \
    printf 'envd_ref=%s\nrootfs_size_mib=%s\nkernel=%s-cube-vz\ninitrd=none\n' \
      "$(cat /rootfs/etc/cubesandbox-envd-ref)" "${ROOTFS_SIZE_MIB}" \
      "${KERNEL_VERSION}" > /out/build-info.txt

COPY --from=kernel-builder /out/kernel /out/kernel

RUN sha256sum /out/kernel /out/rootfs.raw > /out/SHA256SUMS

FROM scratch
COPY --from=packager /out/ /
