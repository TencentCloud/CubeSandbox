---
title: Templates Troubleshooting
lang: en-US
---

# Templates Troubleshooting

| Title | Description | Related Issues |
| --- | --- | --- |
| Custom `tpl create-from-image` keeps timing out | Two common root causes: (1) the custom image either ships no `envd` or doesn't start it at container startup — the default readiness probe hits `envd` at `49983/health`, so without it the probe `connection refused` until timeout; (2) the host is in a nested-virt environment (e.g. AWS EC2) — missing instruction-set bits (XSAVE family) panic the MicroVM, and doubled VM-exits on page faults slow the in-guest agent enough to blow `VsockServerReady` / probe budget. Fixes: follow the [Bring Your Own Image](https://cubesandbox.com/guide/tutorials/bring-your-own-image.html) tutorial for the image; switch to PVM deployment to avoid nested virt. | [#312](https://github.com/TencentCloud/CubeSandbox/issues/312), [#95](https://github.com/TencentCloud/CubeSandbox/issues/95), [#94](https://github.com/TencentCloud/CubeSandbox/issues/94), [#161](https://github.com/TencentCloud/CubeSandbox/issues/161), [#253](https://github.com/TencentCloud/CubeSandbox/issues/253) |
| Template build fails due to insufficient disk space | Building a template requires unpacking the OCI image and writing it to disk, which consumes a lot of temporary space. When the partition holding `/tmp`, `/data/cubelet` or `/usr/local/services/cubetoolbox/` runs low, the template can stall at `UNPACKING` / `BUILDING_EXT4`, or surface as mkfs.ext4 errors such as directory block checksum mismatch or "Ext2 inode is not a directory". | [#240](https://github.com/TencentCloud/CubeSandbox/issues/240), [#251](https://github.com/TencentCloud/CubeSandbox/issues/251) |
