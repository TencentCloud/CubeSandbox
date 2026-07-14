#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin

mount -o remount,rw /
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run /tmp
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
chmod 1777 /tmp
dmesg -n 1 2>/dev/null || true
modprobe vmw_vsock_virtio_transport

echo "CUBEVZ_LIFECYCLE_AGENT_START"
exec /usr/local/sbin/cube-vz-agent
