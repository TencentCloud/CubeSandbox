---
title: Template Creation Times Out When the Sandbox CIDR Overlaps the LAN
author: luzhixing12345
date: 2026-05-20
tags:
  - deployment
  - networking
  - one-click
lang: en-US
---

# Template Creation Times Out When the Sandbox CIDR Overlaps the LAN

## Symptom

Template creation fails during the `CREATING_TEMPLATE` phase:

```bash
cubemastercli run fail: template tpl-xx creation failed: context deadline exceeded
```

Or `cube-bench` reports errors during a benchmark run:

```bash
~/CubeSandbox/examples/cube-bench$ ./bin/cube-bench -c 20 -n 200
...
╭────────────────────────────────────────────────╮ ╭────────────────────────────────────────────────────────────────────────────────────────────────╮
│  Live Stats                                    │ │  Recent Operations                                                                             │
│                                                │ │                                                                                                │
│   Completed    54 / 200                        │ │   #  74  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Errors       20                              │ │   #  73  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   QPS          2.4 req/s                       │ │   #  72  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Avg Create   1254 ms                         │ │   #  69  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Avg Delete   864 ms                          │ │   #  70  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Elapsed      29.1s                           │ │   #  71  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│                                                │ │   #  68  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
╰────────────────────────────────────────────────╯ │   #  67  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
                                                   │                                                                                                │
                                                   ╰────────────────────────────────────────────────────────────────────────────────────────────────╯

create HTTP 500: {"code":500,"message":"CubeMaster returned error code 130595: context deadline exceeded"}
```

## Environment

- Cube Sandbox version: e29453
- Deployment mode: bare-metal or local physical-machine deployment
- Host OS / kernel: Ubuntu 22.04 / Linux 6.6
- Related components: `Cubelet`, persistent TAP devices

## Root Cause

On a bare-metal or local physical-machine deployment, Cube creates many persistent TAP devices named like `z192.168.0.x` or `z192.168.1.x`.

The default configuration is in `Cubelet/config/config.toml`. By default, Cube creates 500 TAP devices from the `192.168.0.0/18` CIDR:

```toml
[plugins]
  [plugins."io.cubelet.internal.v1.network"]
    object_dir = "/usr/local/services/cubetoolbox/cube-vs/network"
    eth_name = "eth0"
    tap_init_num = 500
    cidr = "192.168.0.0/18"
```

Cube's default sandbox CIDR is `192.168.0.0/18`. If the host LAN also uses a related range, such as `192.168.1.x`, sandbox addresses can overlap with the real LAN, causing routing and port probing failures.

After reproducing the failure with `./bin/cube-bench -c 20 -n 200`, check the Cubelet log at `/data/log/Cubelet/Cubelet-req.log`:

```bash
$ rg 'PortBindingFailed|probe \\[|Create fail|RunCubeSandboxRequest|sandboxIP|port_mappings]' /data/log/Cubelet/Cubelet-req.log | jq
{
  "CalleeEndpoint": "",
  "CalleeAction": "Create",
  "Action": "Create",
  "InstanceId": "16157c528b224e9eacc6307a2af5705e",
  "RequestId": "069f93aa-927d-4436-aac0-cc0aa8a89ca9",
  "@timestamp": "2026-05-20T10:10:16.896601536-04:00",
  "InstanceType": "cubebox",
  "Callee": "cubebox",
  "Version": "release",
  "CodeLine": "",
  "FunctionType": "cubebox",
  "Caller": "cubebox-service",
  "Namespace": "default",
  "RetCode": 0,
  "LogContent": "[cubebox] fail:PortBindingFailed The initialization timeout or detecting 192.168.1.40 port failed.",
  "LocalIp": "192.168.1.123",
  "Module": "Cubelet",
  "LogLevel": "ERROR"
}
{
  "InstanceId": "",
  "Callee": "workflow",
  "RetCode": 130459,
  "Namespace": "default",
  "Module": "Cubelet",
  "@timestamp": "2026-05-20T10:10:16.896681441-04:00",
  "LogLevel": "ERROR",
  "CalleeEndpoint": "",
  "FunctionType": "cubebox",
  "Version": "release",
  "InstanceType": "cubebox",
  "LogContent": "Create fail:requestID:\"069f93aa-927d-4436-aac0-cc0aa8a89ca9\"
  ret:{
    ret_code:PortBindingFailed
    ret_msg:\"The initialization timeout or detecting 192.168.1.40 port failed.\"} 
    sandboxID:\"16157c528b224e9eacc6307a2af5705e\"
    sandboxIP:\"192.168.1.40\" 
    port_mappings:{container_port:49983 host_port:20588}
    port_mappings:{container_port:49999 host_port:20589}",
  "Caller": "cubebox-service",
  "CalleeAction": "Create",
  "Action": "Create",
  "LocalIp": "192.168.1.123",
  "CodeLine": "",
  "RequestId": "069f93aa-927d-4436-aac0-cc0aa8a89ca9"
}
```

If the host has overlapping routes at the same time:

```bash
$ ip route
192.168.0.0/18 dev cube-dev proto kernel scope link src 192.168.0.1
192.168.1.0/24 dev enp56s0f0 proto kernel scope link src 192.168.1.123 metric 100
```

`192.168.1.0/24` is more specific than `/18`, so accessing `192.168.1.40` may go through the real physical NIC `enp56s0f0` instead of Cube's `cube-dev` / TAP path, and Cubelet cannot reach the real sandbox when probing the sandbox port.

## Resolution

Stop the services first:

```bash
sudo systemctl stop 'cube-sandbox-*.target'
```

Change the Cubelet network CIDR to a range that does not overlap with the host LAN. For example, change it to `172.31.64.0/18`:

```bash
sudo sed -i 's#cidr = "192.168.0.0/18"#cidr = "172.31.64.0/18"#' \
  /usr/local/services/cubetoolbox/Cubelet/config/config.toml
```

Remove the old persistent TAP devices and the `cube-dev` interface:

```bash
sudo ip link delete cube-dev 2>/dev/null || true
ip tuntap show | awk -F: '/^z[0-9]+\./{print $1}' \
  | xargs -r -n1 -I{} sudo ip tuntap del dev {} mode tap
```

Restart the services:

```bash
sudo systemctl start 'cube-sandbox-*.target'
```

After the sandbox CIDR no longer overlaps with the host LAN, recreate the template. The template creation should complete successfully.

## Related: Changing the CIDR (Residual `cube-dev`)

Stopping Cube Sandbox does NOT remove the `cube-dev` dummy interface or the persistent `z<ip>` TAP devices — they linger until a reboot or manual cleanup. A same-CIDR reinstall reuses the existing `cube-dev` automatically, no action needed; but **changing the CIDR** while a residual `cube-dev` overlaps the new range is rejected, because it is disruptive:

```
[one-click] ERROR: CUBE_SANDBOX_NETWORK_CIDR '192.168.0.0/17' overlaps an existing cube-dev network (192.168.0.0/18).

  Changing the sandbox CIDR on a host that already has a cube network is
  disruptive: the old cube-dev and the persistent z* TAP devices are left
  stale. A reboot alone is NOT enough -- the systemd target is enabled and
  network-agent rebuilds the old network from config.toml on boot.

  To change the CIDR, fully reset the cube network first:
    sudo systemctl stop 'cube-sandbox-*.target'
    sudo ip link delete cube-dev 2>/dev/null || true
    ip tuntap show | awk -F: '/^z[0-9]+\./{print $1}' \
      | xargs -r -n1 -I{} sudo ip tuntap del dev {} mode tap
  then re-run install with the new CIDR.

  Or keep the existing CIDR (192.168.0.0/18) to reuse the current network.

  To bypass this check (not recommended), set:
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1
```

A reboot alone is NOT enough — the systemd target is enabled, so on boot `network-agent` rebuilds `cube-dev` and the TAP devices from `config.toml`. Perform the deterministic reset shown in the error, then re-run the installer with the new `CUBE_SANDBOX_NETWORK_CIDR`. If the new CIDR does not overlap the residual `cube-dev`, the pre-flight allows it and `network-agent` reconciles `cube-dev` to the new network.
