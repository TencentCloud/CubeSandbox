#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Test-only bootstrap. The caller must create a dedicated privileged container
# with private PID/cgroup/mount namespaces; never run this on the host.
set -euo pipefail
[[ $(cat /proc/self/cgroup) == '0::/' ]]
[[ $(stat -f -c %T /sys/fs/cgroup) == cgroup2fs ]]
mount -o remount,rw /sys/fs/cgroup
mkdir /sys/fs/cgroup/test-runner
mapfile -t runner_pids < /sys/fs/cgroup/cgroup.procs
for pid in "${runner_pids[@]}"; do
    echo "$pid" > /sys/fs/cgroup/test-runner/cgroup.procs
done
echo '+cpu +memory' > /sys/fs/cgroup/cgroup.subtree_control
exec setpriv --bounding-set=-sys_time --inh-caps=-sys_time --ambient-caps=-sys_time "$@"
