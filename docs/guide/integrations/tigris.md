---
title: Tigris Volume Integration Guide
author: davidmyriel
date: 2026-07-31
tags:
  - integration
  - tigris
  - volume
  - storage
  - s3
lang: en-US
---

# Tigris Volume Integration Guide

[Tigris](https://www.tigrisdata.com/) is S3-compatible object storage with a single global endpoint. This guide wires it into CubeSandbox through the generic [S3 Volume Plugin](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3), so sandboxes get persistent storage that survives sandbox teardown and is shared across nodes.

The plugin itself is vendor-neutral — **s3fs** for the mount, the **AWS CLI** for the control plane. This guide covers the Tigris-specific parts: account, bucket, access keys, and the two config values that point the plugin at Tigris.

## Integration Target and Version

| Item | Version |
|------|---------|
| Cube platform | **≥ 0.6.0** (CubeMaster, CubeAPI, Cubelet) — the Volume framework landed in v0.6.0 |
| Python SDK `cubesandbox` | **≥ 0.6.0** |
| S3 Volume Plugin | [`examples/volume/s3`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3) |
| s3fs (`s3fs-fuse`) | ≥ 1.90, `amd64` and `arm64` |
| Tigris | S3 API, endpoint `https://t3.storage.dev` |

## Prerequisites

- A running CubeSandbox deployment with CubeMaster, Cubelet, and CubeAPI (usually `http://<node>:3000`), plus a sandbox template ID.
- The S3 Volume Plugin installed and registered under driver name `s3` — follow §§1–5 of the [plugin walkthrough](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3) once; everything there is provider-independent.
- A [Tigris bucket](https://www.tigrisdata.com/docs/buckets/create-bucket/) and an [access key pair](https://www.tigrisdata.com/docs/iam/create-access-key/) with Editor permission on it. Key IDs start with `tid_`, secrets with `tsec_`.

::: tip Works on ARM64
The COS backend cannot run on ARM64 — upstream cosfs ships `amd64` packages only, so [`docker-install-volume-deps.sh`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/scripts/docker-install-volume-deps.sh) skips it on `arm64`. The S3 plugin uses s3fs, which is packaged for `arm64` by both Debian/Ubuntu and EPEL, so this integration runs on ARM64 hosts unchanged.
:::

## Integration Steps

### 1. Create the Tigris bucket and access keys

In the [Tigris dashboard](https://console.tigris.dev/), create a bucket (avoid dots in the name — s3fs's virtual-hosted addressing breaks TLS for dotted names) and an access key pair with Editor permission scoped to that bucket.

### 2. Point the plugin at Tigris

Edit `volume-s3.conf` on the CubeMaster and Cubelet nodes:

```ini
# volume-s3.conf — root-owned, chmod 600 (holds a secret in plaintext)
ACCESS_KEY_ID=tid_xxx
SECRET_ACCESS_KEY=tsec_xxx
BUCKET=my-cube-volumes
ENDPOINT=https://t3.storage.dev
REGION=auto
```

That's the entire Tigris-specific configuration. Tigris serves one global endpoint and routes to the nearest region itself, so there is no per-region endpoint to pick; `REGION=auto` is only used for SigV4 signing.

### 3. Verify connectivity from CubeMaster

```bash
AWS_ACCESS_KEY_ID=tid_xxx AWS_SECRET_ACCESS_KEY=tsec_xxx AWS_REGION=auto \
  aws s3 ls s3://my-cube-volumes/ --endpoint-url https://t3.storage.dev
```

An empty listing (exit 0) is success. `InvalidAccessKeyId` or `AccessDenied` means the key pair lacks Editor permission on the bucket.

### 4. Restart and confirm registration

```bash
sudo systemctl restart cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -3
```

## Key Code Snippets

Full lifecycle with the Python SDK:

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="s3")

with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from Tigris volume")
    print(sb.files.read("/workspace/hello.txt"))

# Sandbox is gone; the object still lives in Tigris.
Volume.destroy(vol.volume_id)
```

One volume mounted by several sandboxes at once — Cubelet reference-counts per node, so only the first attach mounts:

```python
vol = Volume.create("shared-models", driver="s3")

sb1 = Sandbox.create(volume_mounts={"/models": vol})
sb2 = Sandbox.create(volume_mounts={"/models": vol})   # reuses the same s3fs mount
```

Verify what actually landed in the bucket:

```bash
aws s3 ls "s3://my-cube-volumes/volumes/" --recursive --endpoint-url https://t3.storage.dev
```

## Caveats

- **Credentials stay outside the sandbox.** They live in a root-owned, mode-`600` config on CubeMaster and Cubelet; the microVM only ever sees a mounted filesystem. Do not pass them into templates.
- **Object storage is not a POSIX filesystem.** s3fs emulates one. Random writes rewrite the whole object, `rename` is a copy-then-delete, and there is no locking between nodes. Fine for workspaces, datasets, model weights, and outputs — not for a database file or a build cache with concurrent writers on multiple nodes.
- **RefCount is per node.** Two sandboxes on the same node share one s3fs mount; the same volume on a second node mounts separately.
- **Destroy is irreversible** and deletes the whole `volumes/<id>/` prefix. The API's refcount guard (`DELETE /volumes` returns 409 while any sandbox holds the volume) is what prevents deleting a mounted volume; the realistic hazard is a stale refcount after a node crash, where the cluster-wide count can reach 0 while a node still holds the mount.
- **The official e2b Python SDK will not work** against CubeSandbox — it is hardcoded to the e2b.cloud backend. Use `cubesandbox`, or call the e2b-compatible `/volumes` REST endpoints directly.

## References

- S3 Volume Plugin (the generic plugin this guide configures): [`examples/volume/s3`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3)
- Volume Plugin framework: [`docs/guide/volume-plugin.md`](../volume-plugin.md)
- Host mount alternative for node-local paths: [`docs/guide/persistent-storage.md`](../persistent-storage.md)
- Tigris: <https://www.tigrisdata.com/>
- Tigris documentation: <https://www.tigrisdata.com/docs/>
- Tigris S3 compatibility and AWS CLI setup: <https://www.tigrisdata.com/docs/sdks/s3/aws-cli/>
- s3fs-fuse: <https://github.com/s3fs-fuse/s3fs-fuse>
