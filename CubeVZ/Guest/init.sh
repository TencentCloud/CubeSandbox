#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin
export HOME=/root
export LANG=C.UTF-8
BB=/usr/local/sbin/busybox

$BB mount -o remount,rw /
$BB mountpoint -q /proc || $BB mount -t proc proc /proc
$BB mountpoint -q /sys || $BB mount -t sysfs sysfs /sys
$BB mountpoint -q /dev || $BB mount -t devtmpfs devtmpfs /dev
$BB mkdir -p /dev/pts /run /tmp
$BB mountpoint -q /dev/pts || $BB mount -t devpts devpts /dev/pts
$BB chmod 1777 /tmp
$BB dmesg -n 1 2>/dev/null || true
$BB modprobe vmw_vsock_virtio_transport

$BB ip link set lo up
$BB ip link set eth0 up
$BB udhcpc -q -n -i eth0 -s /usr/local/sbin/cube-vz-udhcpc || true

ENVD_LOG_FILE=/dev/console /usr/local/bin/cube-entrypoint.sh &
/usr/local/sbin/cube-vz-relay 49983 &

ready=0
for _ in $($BB seq 1 200); do
  if $BB wget -q -O /dev/null http://127.0.0.1:49983/health; then
    ready=1
    break
  fi
  $BB sleep 0.05
done
if [ "$ready" -ne 1 ]; then
  echo "CUBEVZ_ENVD_ERROR readiness timed out" >/dev/console
  exit 1
fi

exec /usr/local/sbin/cube-vz-agent
