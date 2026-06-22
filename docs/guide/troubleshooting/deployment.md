---
title: Deployment Troubleshooting
lang: en-US
---

# Deployment Troubleshooting

| Title | Description | Related Issues |
| --- | --- | --- |
| `/data/cubelet` must be on XFS (reflink) | `cubelet` stores container writable layers under `/data/cubelet` and relies on XFS reflink. Deploying on ext4-rooted hosts (Ubuntu / Debian / WSL) makes the one-click pre-flight reject with `not XFS`. Workaround: mount a loopback `.img` formatted as XFS at `/data/cubelet`. For production, attach a dedicated XFS data disk (100–300 GiB). For fresh installs prefer OpenCloudOS 9 / RHEL family. | [#311](https://github.com/TencentCloud/CubeSandbox/issues/311), [#245](https://github.com/TencentCloud/CubeSandbox/issues/245) |
| Template Creation Times Out When the Sandbox CIDR Overlaps the LAN | The one-click deployment defaults the sandbox network to `192.168.0.0/18`. If the host LAN also uses `192.168.1.x`, Cube may allocate sandbox IPs that overlap the physical network, causing template creation or port probing to fail with `context deadline exceeded`. Change the Cubelet CIDR to a non-overlapping range and remove the old TAP devices plus `cube-dev` before restarting. | [Guide](./local-network-cidr-conflict.md) |
| Changing the CIDR (residual `cube-dev`) | Stopping Cube does not remove the `cube-dev` interface or `z*` TAP devices, so they linger after a stop. Changing `CUBE_SANDBOX_NETWORK_CIDR` to a range that overlaps the residual `cube-dev` is rejected by the pre-flight with a deterministic-reset hint (a reboot alone is not enough). A same-CIDR reinstall reuses `cube-dev` automatically. | [Guide](./local-network-cidr-conflict.md#changing-the-cidr-residual-cube-dev) |
| cgroup v2 `cpu` controller not enabled on Ubuntu, cubelet CPU quotas don't take effect | Ubuntu / Debian cloud images don't delegate the cgroup v2 `cpu` controller to child cgroups by default, and `multipathd`'s RT threads make `+cpu` writes fail with `Invalid argument`. See the issue for full repro and fix. | [#366](https://github.com/TencentCloud/CubeSandbox/issues/366) |
