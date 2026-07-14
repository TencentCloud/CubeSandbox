#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin
export HOME=/root
export ENVD_PORT=49983
export EXEC_PORT=49999

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

/usr/local/sbin/cube-vz-exec --host 127.0.0.1 --port "$EXEC_PORT" \
  >>/var/log/cube-vz-exec.log 2>&1 &
echo "CUBEVZ_EXEC_START pid=$! port=$EXEC_PORT"

exec_ready=0
for _ in $(seq 1 100); do
  if curl -fsS -H 'Content-Type: application/json' \
    --data-binary '{"code":"1 + 1","language":"python"}' \
    "http://127.0.0.1:${EXEC_PORT}/execute" 2>/dev/null | grep -Fq '"text":"2"'; then
    exec_ready=1
    break
  fi
  sleep 0.01
done
if [ "$exec_ready" -eq 1 ]; then
  echo "CUBEVZ_EXEC_SELFTEST=ok"
else
  echo "CUBEVZ_EXEC_SELFTEST=error"
  exit 1
fi

/usr/local/sbin/cube-vz-egress >>/var/log/cube-vz-egress.log 2>&1 &
echo "CUBEVZ_EGRESS_START pid=$! port=18080"

echo "CUBEVZ_LIFECYCLE_AGENT_START"
exec /usr/local/sbin/cube-vz-agent
