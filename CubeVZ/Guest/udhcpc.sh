#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

BB=/usr/local/sbin/busybox

case "$1" in
  deconfig)
    $BB ip address flush dev "$interface"
    ;;
  bound|renew)
    $BB ip address flush dev "$interface"
    prefix=24
    if [ -n "${subnet:-}" ]; then
      prefix="$($BB ipcalc -p "$ip" "$subnet" | $BB cut -d= -f2)"
    fi
    $BB ip address add "$ip/$prefix" dev "$interface"
    if [ -n "${router:-}" ]; then
      $BB ip route replace default via "${router%% *}" dev "$interface"
    fi
    : > /etc/resolv.conf
    for server in ${dns:-}; do
      printf 'nameserver %s\n' "$server" >> /etc/resolv.conf
    done
    ;;
esac
