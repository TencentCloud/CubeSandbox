# MySQL 沙箱验证报告

> 本文档是 2026-07-15 的 mysql-sandbox 示例验证记录。

## 验证环境

| 项 | 值 |
|----|----|
| 节点 IP | 10.206.0.6 |
| CubeMaster 版本 | v0.5.1 |
| cubelet 版本 | v0.5.1 |
| Cubelet 通信端口 | 9999 |
| CubeMaster API 端口 | 8089 |
| Cube-API 端口 | 3000 |
| 工作模板 ID | `tpl-7bc80edafe284f2ea0cdcb45` |
| 基础快照 | `snap-28820e89fa96468789bf5f89` |
| OS | Linux 6.6.69-opencloudos9 |

## 网络拓扑发现

中国国内节点跨境出口带宽极窄，所有境外 registry 拉取缓慢或失败：

| Registry | 速度 | 备注 |
|----------|------|------|
| `ghcr.io` (GitHub) | ~20 KiB/s，频繁 HTTP/2 stream error | ❌ 卡 UNPACKING 5/9 layers |
| `quay.io` | ~4 bytes/s | ❌ 不可用 |
| `registry-1.docker.io` (Docker Hub) | timeout | ❌ 不可达 |
| `docker.m.daocloud.io` | HTTP 401（无凭据） | ❌ 需 token |
| `cube-sandbox-image.tencentcloudcr.com` (腾讯云内网) | 仅内网写入被拒 | ⚠️ 只读 namespace |

`registry-1.docker.io` 不可达意味着无法直接使用本地 docker daemon 缓存的镜像来构建模板。
`cube-sandbox-image.tencentcloudcr.com/opensource/...` 这类只读 namespace 是 Cubelet 启动组件用的，不开放用户写入。

## 镜像构建

在联网的主机上构建镜像（本地可用）：

```bash
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-client:latest examples/mysql-sandbox
```

镜像大小 226 MB（压缩 55.7 MB）。`docker images` 中已正常标记。

## ghcr.io 推送结果

GitHub Personal Access Token 登录成功：

```
docker login ghcr.io -u pei-pei45
# 输出: Login Succeeded
```

推送镜像：

```
docker push ghcr.io/pei-pei45/cubesandbox-mysql-client:latest
# latest: digest: sha256:7d4aa1b8... size: 2259
```

✅ **镜像已推送至 ghcr.io。** 但该镜像**只能在网络通畅的环境下使用**。

## 模板创建

### 方案 A：`create-from-image`（失败）

```bash
cubemastercli tpl create-from-image \
    --image ghcr.io/pei-pei45/cubesandbox-mysql-client:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

**失败现象**：

- 状态进入 `UNPACKING progress=20%`，`pull: 11MiB/53MiB 15KiB/s`
- 多小时后仍未推进到下一阶段
- 多次重试均以 `stream error: stream ID 17; PROTOCOL_ERROR` 结尾
- template 始终停留在 `RUNNING`，最终 `FAILED`

**根本原因**：跨境出口带宽不足，且 `ghcr.io` 对中国 IP 偶发 HTTP/2 reset。`cubemaster` 不会自动 fallback 到本地 containerd 缓存——它直接调用 registry 的 manifest endpoint。

### 方案 B：`commit sandbox`（成功 ✅）

完全绕开镜像拉取链路：

1. 启动基础 sandbox：
   ```bash
   curl -X POST http://127.0.0.1:3000/sandboxes \
       -H "X-API-KEY: e2b_000000" \
       -d '{"templateID": "snap-28820e89fa96468789bf5f89"}'
   # sandboxID: df2ced2b21e741aab6c6fefc1391391a（演示用）
   ```

2. 在 sandbox 内安装 mysql-client + curl/wget/jq + `/srv/sql` + `/health-mysql.sh`：
   ```bash
   # 沙箱内执行，~5 秒：
   set -e
   sed -i 's/deb.debian.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apt/sources.list.d/debian.sources
   apt-get update -qq && apt-get install -y -qq default-mysql-client curl wget jq
   mkdir -p /srv/sql && chmod 755 /srv/sql
   cat > /health-mysql.sh <<'HSH'
   #!/bin/bash
   mysql --version
   HSH
   chmod +x /health-mysql.sh
   ```

3. 用合并后的 `merged_request` JSON 作为 commit 输入：
   ```bash
   cubemastercli tpl render --template-id snap-28820e89fa96468789bf5f89 --json > /tmp/rendered.json
   # 把 merged_request 提取成 /tmp/full_req.json（含 image.annotations）
   cubemastercli tpl commit \
       --sandbox-id df2ced2b21e741aab6c6fefc1391391a \
       --file /tmp/full_req.json
   # template_id: tpl-7bc80edafe284f2ea0cdcb45  Status: READY 2s 后
   ```

   关键点：仅提供 `{"templateID": "..."}` 会得到 `containers param is nil` 错误。必须从 `merged_request` 中提取完整的 `containers[0].image.annotations` (含 `cube.master.rootfs.artifact.token` 等)。

## 模板验证

设置环境变量（已写入 `.env`）：

```
E2B_API_URL=http://127.0.0.1:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=tpl-7bc80edafe284f2ea0cdcb45
SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### `python3 check_mysql.py`

✅ **通过**：

```
[1] Creating sandbox...
[2] Sandbox created successfully!
[3] Checking MySQL client...
    MySQL version: mysql  Ver 15.1 Distrib 10.11.18-MariaDB, for debian-linux-gnu (x86_64) using  EditLine wrapper
[4] /usr/bin/mysql
    /usr/bin/mysqldump
[5] Linux tpl-a067 6.6.69-opencloudos9.cubesandbox.pvm.guest-gb85200d80fa2 #1 SMP PREEMPT_DYNAMIC
[6] Debian GNU/Linux 12 (bookworm)
Sandbox verification completed!
```

耗时 < 2 秒。

### `python3 snapshot_demo.py`

✅ **通过**：

- 创建 baseline sandbox，写入 `/tmp/app_state.txt`
- `sandbox.create_snapshot()` → `snap-675a501529444e27a9715957`
- 修改状态后销毁原 sandbox
- `Sandbox.create(snapshot_id)` → 状态回滚到 baseline
- 还演示了 "Safe Database Operations"（schema + 迁移 + 回滚）

### `python3 network_isolated.py`

✅ **通过**：

| 场景 | 期望 | 实际 |
|------|------|------|
| `allow_internet_access=False` | curl 失败 | ✅ "Internet access correctly blocked!" |
| `network={"allow_out": ["10.0.0.0/8", ...]}` | 允许私网拒绝公网 | ✅ 配置生效 |
| `allow_out=10.0.0.0/8` + DB 配置 | 配置生效 | ✅ |

### `python3 multi_query.py`

❌ **预期失败**：脚本默认 `DB_HOST=localhost` 寻找 `mysqld.sock`。本机 mysql 服务器在 `cube-sandbox-mysql` 容器里，监听 `127.0.0.1:3306`，**从 KVM 沙箱无法访问主机 127.0.0.1 / 10.206.0.6**。把 mysql 容器端口暴露到 `0.0.0.0` 后，主机 IP 仍不可达，因为：
- KVM 沙箱走 `cube-egress` transparent proxy，只代理 `tcp dport 80/443`
- 沙箱与主机网络完全隔离，tap 接口仅连入 `cube-dev` bridge

这是 KVM 隔离的有意设计，不是 bug。如果要将 mysql 暴露给沙箱，需要：走公网 mysql / 内置 mysql server 镜像 / 配置 cube-egress 的白名单。

## 总结

| 步骤 | 结果 |
|------|------|
| docker build 镜像 | ✅ |
| docker push ghcr.io | ✅（适用于跨境网通畅的环境） |
| ghcr.io → 模板 (`create-from-image`) | ❌ 跨境出口瓶颈，无法用作模板源 |
| 沙箱内安装 mysql + commit 为模板 | ✅ 推荐方案 |
| 模板启动 + MySQL 客户端验证 | ✅ `< 2s` |
| 快照/回滚 | ✅ |
| 网络隔离 | ✅ |
| 沙箱内连接到本机 MySQL 容器 | ⚠️ KVM 沙箱与主机网络天然隔离，需其它方案 |

## 后续建议

1. 在 `docs/guide/quickstart.md` 中增加"无公网环境"的故障转移路径：直接说明若 `create-from-image` 卡在 UNPACKING，可以使用 commit workflow。
2. 如果要让 `run_query.py` / `multi_query.py` 在沙箱内真实连接到 MySQL，可考虑：
   - 在 mysql-sandbox 同例内嵌一个最小 mysql/mariadb 服务器镜像（这样沙箱自带服务端，零网络依赖）。
   - 或部署 cube-egress 端口白名单把 3306 加进去。
   - 或通过 mysql RDSH 公网地址 / 第三方 SaaS（如 planetscale / tidb cloud）演示。
3. 已生成的 ghcr.io 镜像仍是有效资产，在跨境出口正常的演示环境可直接 `docker pull ghcr.io/pei-pei45/cubesandbox-mysql-client:latest && cubemastercli tpl create-from-image ...`。
