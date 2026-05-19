---
title: Deployment Troubleshooting
lang: en-US
---

# Deployment Troubleshooting

| Title | Description | Related Issues |
| --- | --- | --- |
| `/data/cubelet` must be on XFS (reflink) | `cubelet` stores container writable layers under `/data/cubelet` and relies on XFS reflink. Deploying on ext4-rooted hosts (Ubuntu / Debian / WSL) makes the one-click pre-flight reject with `not XFS`. Workaround: mount a loopback `.img` formatted as XFS at `/data/cubelet`. For production, attach a dedicated XFS data disk (100–300 GiB). For fresh installs prefer OpenCloudOS 9 / RHEL family. | [#311](https://github.com/TencentCloud/CubeSandbox/issues/311), [#245](https://github.com/TencentCloud/CubeSandbox/issues/245) |
