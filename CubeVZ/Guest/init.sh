#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin
export HOME=/root
export LANG=C.UTF-8
BB=/usr/local/sbin/busybox

$BB mount -t proc proc /proc
$BB mount -o remount,rw /
$BB mountpoint -q /sys || $BB mount -t sysfs sysfs /sys
$BB mountpoint -q /dev || $BB mount -t devtmpfs devtmpfs /dev
$BB mkdir -p /dev/pts /run /tmp
$BB mountpoint -q /dev/pts || $BB mount -t devpts devpts /dev/pts
$BB chmod 1777 /tmp
$BB dmesg -n 1 2>/dev/null || true
init_ready_s="$($BB cut -d ' ' -f 1 /proc/uptime)"

$BB ip link set lo up
$BB ip link set eth0 up
# envd uses vsock, so DHCP does not block lifecycle readiness. If VZNAT drops
# an initial discover during a burst, udhcpc continues retrying in background.
$BB udhcpc -q -b -t 3 -T 1 -A 1 \
  -i eth0 -s /usr/local/sbin/cube-vz-udhcpc &

ENVD_LOG_FILE=/dev/console /usr/local/bin/cube-entrypoint.sh &
relay_ready_file=/run/cube-vz-relay.ready
$BB rm -f "$relay_ready_file"
/usr/local/sbin/cube-vz-relay 49983 "$relay_ready_file" &
relay_pid=$!

ready=0
for _ in $($BB seq 1 1000); do
  if ! $BB kill -0 "$relay_pid" 2>/dev/null; then
    echo "CUBEVZ_RELAY_ERROR relay exited before readiness" >/dev/console
    exit 1
  fi
  if [ -s "$relay_ready_file" ] \
    && $BB wget -q -O /dev/null http://127.0.0.1:49983/health; then
    ready=1
    break
  fi
  $BB sleep 0.01
done
if [ "$ready" -ne 1 ]; then
  echo "CUBEVZ_ENVD_ERROR readiness timed out" >/dev/console
  exit 1
fi
envd_ready_s="$($BB cut -d ' ' -f 1 /proc/uptime)"

guest_ready_s="$($BB cut -d ' ' -f 1 /proc/uptime)"

exec /usr/local/sbin/cube-vz-agent \
  "init_s=$init_ready_s envd_s=$envd_ready_s ready_s=$guest_ready_s"
