# COS Volume Plugin (rpc)

**Plugin type:** `rpc` (long-running gRPC process; transport via `socket_path`: Unix / TCP)  
**Driver name:** `cos-rpc`

| Side | Hooks | This example uses |
|------|-------|-------------------|
| CubeMaster (Controller) | Create / Destroy | [COS Go SDK](https://cloud.tencent.com/document/product/436/31215) |
| Cubelet (Node) | Attach / Detach | [cosfs](https://cloud.tencent.com/document/product/436/10976) |

Compared to [binary](../binary/): Controller uses **Go SDK** instead of **coscmd**; Node still uses **cosfs**.

> Shared COS docs: [`../README.md`](../README.md)  
> Framework: [docs/guide/volume-plugin.md](../../../../docs/guide/volume-plugin.md)

> **Note**  
> This example **demonstrates rpc plugin usage only**; Attach / Detach run locally on the Cubelet node, so production gRPC servers belong on the Node as well.  
> **Steps below assume a single-host setup** (CubeMaster and Cubelet on one machine); split roles across hosts as needed.

中文文档：[README.zh.md](README.zh.md)

---

## Before you start

| Step | Content |
|------|---------|
| 1 | [../README.md — Prerequisites](../README.md#prerequisites) |
| 2 | **Cubelet node:** install cosfs — [§1](../README.md#1-install-dependencies) |
| 3 | **Go 1.24+** ([go.mod](go.mod) declares `go 1.24.8`), build `cube-volume-cos-rpc` in this directory |
| 4 | Configure CubeMaster + Cubelet `socket_path` and plugin `SOCKET` (`/run/cube-volume-cos-rpc.sock`) |
| 5 | SDK: `Volume.create(..., driver="cos-rpc")` |

**No coscmd** in this example; Create/Destroy use the COS Go SDK (`go mod`, [31215](https://cloud.tencent.com/document/product/436/31215)).

---

## 1. cosfs (Cubelet node)

Attach/Detach use cosfs. **Cubelet node only** — see [../README.md §1](../README.md#1-install-dependencies).

```bash
sudo ../install-deps.sh --cosfs
ls /dev/fuse && which cosfs && cosfs --version
```

Official doc: [cosfs tool](https://cloud.tencent.com/document/product/436/10976)

---

## 2. COS Go SDK (Controller)

- Dependency in [go.mod](go.mod): `github.com/tencentyun/cos-go-sdk-v5`
- Keys, bucket, region: `/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf` (same directory as the rpc binary; optional copy under `Cubelet/plugin/` when cosfs runs on the node)
- China mirror: `GOPROXY=https://goproxy.cn,direct`

Local dev `replace` for Cubelet (path relative to this directory):

```go
require github.com/tencentcloud/CubeSandbox/Cubelet v0.0.0

replace github.com/tencentcloud/CubeSandbox/Cubelet => ../../../../Cubelet
```

---

## 3. Build and run

Requires **Go 1.24+** (`go.mod` declares `go 1.24.8`).

```bash
PREFIX=/usr/local/services/cubetoolbox/CubeMaster/plugin
mkdir -p "$PREFIX"
cd examples/volume/cos/rpc
go build -o "$PREFIX/cube-volume-cos-rpc" ./cmd/cube-volume-cos-rpc

sudo cp volume-cos.conf.example "$PREFIX/volume-cos.conf"
# Edit SECRET_ID, SECRET_KEY, BUCKET, REGION; SOCKET is set in the example
sudo chmod 600 "$PREFIX/volume-cos.conf"

"$PREFIX/cube-volume-cos-rpc" serve
```

**systemd example:**

```ini
[Unit]
Description=COS Volume Plugin (rpc)
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos-rpc serve
EnvironmentFile=-/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Install the unit file before `enable` (a one-liner with `serve &` + `systemctl` will fail with *Unit … could not be found*):

```bash
sudo cp cube-volume-cos-rpc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cube-volume-cos-rpc
```

`SOCKET` in `volume-cos.conf` must be the **same path** as CubeMaster / Cubelet `socket_path` (standard: `/run/cube-volume-cos-rpc.sock`).

Implementation: [internal/grpcsrv/server.go](internal/grpcsrv/server.go)

---

## 4. Configure CubeMaster and Cubelet

**CubeMaster** `conf.yaml`:

```yaml
volume_plugins:
  - name: cos-rpc
    type: rpc
    socket_path: /run/cube-volume-cos-rpc.sock
```

**Cubelet** `config.toml`:

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos-rpc"
  type        = "rpc"
  socket_path = "/run/cube-volume-cos-rpc.sock"
```

Can coexist with binary (`cos` / `cos-rpc`); see [binary/README.md](../binary/README.md).

Restart CubeMaster and Cubelet after editing config, then confirm plugins loaded ([§5](#5-end-to-end-verification)).

---

## 5. End-to-end verification

After restart, check startup logs (`grep -a` because log files may contain binary data):

```bash
# CubeMaster — one line per plugin
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5

# Cubelet — one line per plugin after Init
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

Expected for this example:

```text
[volume] registered rpc plugin "cos-rpc" at /run/cube-volume-cos-rpc.sock
[plugin_volume] initialized rpc plugin "cos-rpc" at /run/cube-volume-cos-rpc.sock
```

Also confirm the rpc daemon: `systemctl is-active cube-volume-cos-rpc`.

### SDK smoke test

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="cos-rpc")

sb = Sandbox.create(
    volume_mounts={"/mnt/data": vol},
)
try:
    sb.files.write("/mnt/data/hello.txt", "via cos-rpc")
    print(sb.files.read("/mnt/data/hello.txt"))
finally:
    sb.kill()

Volume.destroy(vol.volume_id)
```

---

## vs binary

| | [binary](../binary/) | rpc (this example) |
|---|---------------------|-------------------|
| Type | binary | rpc |
| Controller | [coscmd](https://cloud.tencent.com/document/product/436/6883) | [COS Go SDK](https://cloud.tencent.com/document/product/436/31215) |
| Node | [cosfs](https://cloud.tencent.com/document/product/436/10976) | [cosfs](https://cloud.tencent.com/document/product/436/10976) |
| driver | `cos` | `cos-rpc` |

cosfs / mntns troubleshooting: [binary/README.md](../binary/README.md#create-sandbox-with-mount).

## References

- [Shared COS docs](../README.md)
- [cosfs](https://cloud.tencent.com/document/product/436/10976) · [COS Go SDK](https://cloud.tencent.com/document/product/436/31215)
- Proto: `Cubelet/api/services/volumeplugin/v1/volumeplugin.proto`
- [Volume Plugin framework](../../../../docs/guide/volume-plugin.md)
