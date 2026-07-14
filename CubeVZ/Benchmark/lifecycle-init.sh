#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin
export HOME=/root
export ENVD_PORT=49983

mount -o remount,rw /
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run /tmp
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
chmod 1777 /tmp
dmesg -n 1 2>/dev/null || true
modprobe vmw_vsock_virtio_transport
modprobe virtio_net 2>/dev/null || true
modprobe ip_tables 2>/dev/null || true
modprobe iptable_filter 2>/dev/null || true
modprobe nf_conntrack 2>/dev/null || true
ip link set lo up
if ip link show eth0 >/dev/null 2>&1; then
  ip link set eth0 up
  udhcpc -i eth0 -q -n -t 5
fi

/usr/bin/envd -isnotfc -port "$ENVD_PORT" 2>&1 | tee -a /var/log/envd.log >/dev/console &
echo "CUBEVZ_ENVD_START pid=$! port=$ENVD_PORT"

/usr/local/sbin/cube-vz-egress >>/var/log/cube-vz-egress.log 2>&1 &
echo "CUBEVZ_EGRESS_START pid=$! port=18080"

echo "CUBEVZ_LIFECYCLE_AGENT_START"
exec /usr/local/sbin/cube-vz-agent
