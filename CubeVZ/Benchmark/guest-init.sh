#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin
export HOME=/root
export LANG=C

mount -o remount,rw /
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run /tmp /var/lib/cube-vz-bench
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
chmod 1777 /tmp
dmesg -n 1 2>/dev/null || true

read -r guest_uptime_seconds _ </proc/uptime

echo "CUBEVZ_BENCH_BEGIN schema=1"
echo "guest_arch=$(uname -m)"
echo "guest_kernel=$(uname -r)"
echo "guest_vcpus=$(getconf _NPROCESSORS_ONLN)"
echo "guest_memory_kib=$(awk '/MemTotal/ { print $2 }' /proc/meminfo)"
echo "guest_boot_seconds=${guest_uptime_seconds}"
echo "root_device=$(awk '$2 == "/" { print $1 }' /proc/mounts)"
echo "root_filesystem=$(awk '$2 == "/" { print $3 }' /proc/mounts)"

echo "CUBEVZ_BENCH_CPU_BEGIN"
sysbench cpu \
  --threads="$(getconf _NPROCESSORS_ONLN)" \
  --time=5 \
  --cpu-max-prime=20000 \
  run
echo "CUBEVZ_BENCH_CPU_END"

echo "CUBEVZ_BENCH_MEMORY_BEGIN"
sysbench memory \
  --threads="$(getconf _NPROCESSORS_ONLN)" \
  --time=5 \
  --memory-block-size=1M \
  --memory-total-size=100G \
  run
echo "CUBEVZ_BENCH_MEMORY_END"

cd /var/lib/cube-vz-bench
echo "CUBEVZ_BENCH_FILEIO_BEGIN"
sysbench fileio \
  --file-total-size=256M \
  --file-num=4 \
  --file-block-size=16K \
  prepare >/dev/null
sysbench fileio \
  --threads="$(getconf _NPROCESSORS_ONLN)" \
  --time=5 \
  --file-total-size=256M \
  --file-num=4 \
  --file-block-size=16K \
  --file-test-mode=rndrw \
  --file-io-mode=sync \
  --file-extra-flags=direct \
  --file-fsync-freq=0 \
  run
sysbench fileio \
  --file-total-size=256M \
  --file-num=4 \
  cleanup >/dev/null
echo "CUBEVZ_BENCH_FILEIO_END"

sync
echo "CUBEVZ_BENCH_END"
poweroff -f

# The reboot syscall above should not return. Keep PID 1 alive if it does so
# the host can diagnose the failure from the serial console.
echo "CUBEVZ_BENCH_ERROR poweroff_returned"
while :; do sleep 3600; done
