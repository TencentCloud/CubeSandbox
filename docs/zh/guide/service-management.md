# 服务管理与日志

本页面面向**已经把 CubeSandbox 装好**、想要在日常运维中管好这套服务的用户。

读完本页你会知道：

- 这台机器上具体跑了哪些 systemd 服务，它们之间的依赖关系
- 改了配置之后，要重启哪几个服务才能让它生效
- 服务跑挂了应该怎么排查
- 业务日志、启动期日志、容器内日志各在哪里，以及它们的边界
- 完整下线 / 重启整机栈的姿势

::: tip 适用范围
本页对应**新版（systemd 托管）的一键安装包**。如果你的机器上还能看到 `up-with-deps.sh` / `down-with-deps.sh` 这类脚本作为日常入口，那是 pre-systemd 老版本，重新执行最新一键安装包即可平滑升级（安装器内置了对老部署的接管逻辑）。
:::

## TL;DR 一分钟备忘

```bash
# 1. 看整台机器上 cube 系列服务都还活着吗
sudo systemctl --no-legend list-units 'cube-sandbox-*'

# 2. 改了配置 → 重启对应服务（最常见的几个）
sudo systemctl restart cube-sandbox-cube-api.service
sudo systemctl restart cube-sandbox-cubemaster.service
sudo systemctl restart cube-sandbox-cubelet.service

# 3. 看业务行为日志（请求/统计/审计/VMM）—— 全都在 /data/log/，不在 journal
sudo tail -F /data/log/Cubelet/Cubelet-req.log
sudo tail -F /data/log/CubeMaster/cubemaster-req.log
sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log
sudo tail -F /data/log/CubeVmm/vmm.log              # 沙箱 VMM 创建过程

# 4. 看启动失败 / 进程异常退出 → journalctl
sudo journalctl -u cube-sandbox-cube-api.service -n 200 --no-pager

# 5. 一键打包诊断信息（含 /data/log 的 tail + 配置 + dmesg + 进程快照）
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh
```

::: warning 业务日志在 `/data/log/`，不在 `journalctl`
这是新用户最容易踩的一点：CubeSandbox 各组件**只把启动期 stdout/stderr 留给 journal**，请求 / 调度 / 统计 / 审计 / VMM 创建过程等业务日志**全部直接写到 `/data/log/<Module>/`**。如果你想看「最近一小时谁创建了沙箱」，要去 `/data/log/`，不要去 `journalctl`。
:::

## 服务总览

新版一键安装会把 14 个 systemd 单元注册到 `/etc/systemd/system/`，并按节点角色聚合到两个 target 之下。

### 角色聚合 target

| Target | 作用 | 节点角色 |
|---|---|---|
| `cube-sandbox-control.target` | 控制节点（默认 all-in-one）的全部服务 | `control` |
| `cube-sandbox-compute.target` | 计算节点的最小子集 | `compute` |

::: tip 这是怎么生效的
Target 通过 `Wants=` 列出自己要拉起的 service；service 通过 `PartOf=` 反向声明属于哪个 target。所以 `systemctl stop cube-sandbox-control.target` 会把所有 `PartOf=cube-sandbox-control.target` 的 service 一起停掉，不需要你逐个写名字。
:::

### 服务清单

| 单元 | 进程形态 | 端口 / 监听 | 出现在 | 上游依赖 |
|---|---|---|---|---|
| `cube-sandbox-mysql.service` | Docker 容器 | `3306` | control | docker |
| `cube-sandbox-redis.service` | Docker 容器 | `6379` | control | docker |
| `cube-sandbox-cubemaster.service` | 宿主机进程 | `8089` | control | mysql, redis |
| `cube-sandbox-cube-api.service` | 宿主机进程 | `3000`（E2B 兼容 API） | control | cubemaster |
| `cube-sandbox-network-agent.service` | 宿主机进程 | `19090`（health） | control / compute | network |
| `cube-sandbox-cubelet.service` | 宿主机进程 | `9999`（gRPC） | control / compute | network-agent + `/data/cubelet`（XFS） |
| `cube-sandbox-coredns.service` | Docker 容器 | `127.0.0.54:53` 或 `169.254.254.53:53` | control | docker |
| `cube-sandbox-cube-proxy.service` | Docker 容器 | `443`（TLS）/ `80` | control | docker, redis |
| `cube-sandbox-dns.service` | oneshot（无常驻进程） | — | control | coredns（`BindsTo`）|
| `cube-sandbox-webui.service` | Docker 容器 | `12088` | control | docker, cube-api |

### 启动依赖关系（控制节点）

```text
docker.service
   ├─ mysql.service ─┐
   ├─ redis.service ─┼─ cubemaster.service ─ cube-api.service ─ webui.service
   │                 └─ cube-proxy.service
   └─ coredns.service ─ dns.service (oneshot, BindsTo coredns)

network-online.target
   └─ network-agent.service ─ cubelet.service
```

依赖只通过 `After=` / `Wants=` 表达启动顺序；运行期某个上游挂了**不会**自动把下游也带翻 —— 所以 cubelet 不会因为 cube-api 挂了而被一起重启，反过来也是一样。

## 重启服务

### 场景 A：改了配置，想让单个服务生效

最常见的两种配置入口：

- 顶层环境：`/usr/local/services/cubetoolbox/.one-click.env`
- 组件原生配置：`Cubelet/config/config.toml`、`Cubelet/dynamicconf/conf.yaml`、`CubeMaster/conf.yaml`、`network-agent/network-agent.yaml`、`cubeproxy/global.conf`、`coredns/Corefile`

改完之后，重启**直接读这份配置的那个服务**：

```bash
# 改了 cubelet 配置
sudo systemctl restart cube-sandbox-cubelet.service

# 改了 cubemaster 配置
sudo systemctl restart cube-sandbox-cubemaster.service

# 改了 .one-click.env 中的 CUBE_API_*
sudo systemctl restart cube-sandbox-cube-api.service

# 改了 cubeproxy/global.conf
sudo systemctl restart cube-sandbox-cube-proxy.service

# 改了 coredns/Corefile
sudo systemctl restart cube-sandbox-coredns.service
```

::: warning 改 systemd unit 文件本身的情况
如果你直接动了 `/etc/systemd/system/cube-sandbox-*.service` 文件本身，需要先 reload，systemd 才会读到新内容：

```bash
sudo systemctl daemon-reload
sudo systemctl restart cube-sandbox-<service>.service
```

但如果你只是改了 helper 脚本（`/usr/local/services/cubetoolbox/scripts/systemd/*.sh`），不需要 `daemon-reload`，下一次 `restart` 就会重新拉起脚本生效。
:::

### 场景 B：服务挂了 / 反复重启

每个 service 都配了 `Restart=on-failure`，进程崩一次会自动重启。但如果反复 fail，需要先看清楚原因再重启。

#### 1. 先看一眼当前状态

```bash
sudo systemctl status cube-sandbox-cube-proxy.service --no-pager
```

重点看：

- `Active: failed` / `Active: activating (start-post)` 是不是还在尝试
- `Restart Counter` 是否反复增长（卡在重启循环）
- 输出末尾自动带的最近 10 条 journal

#### 2. 看启动期日志

```bash
sudo journalctl -u cube-sandbox-cube-proxy.service -n 200 --no-pager
```

适合定位：脚本写法问题、ExecStart 报错、容器拉不到镜像、`apk` / `apt` 网络问题、ExecStartPost 健康检查超时。

#### 3. 看业务日志

如果服务能起来但行为异常，**业务日志在 `/data/log/`，不在 journal**：

```bash
sudo tail -200 /data/log/Cubelet/Cubelet-req.log
sudo tail -200 /data/log/CubeMaster/cubemaster-req.log
sudo tail -200 /data/log/CubeAPI/cube-api-$(date +%F).log
```

#### 4. 重置 failed 计数后再启

```bash
sudo systemctl reset-failed cube-sandbox-cube-proxy.service
sudo systemctl restart cube-sandbox-cube-proxy.service
```

### 场景 C：整套重启 / 整机维护后复位

```bash
# 控制节点
sudo systemctl restart cube-sandbox-control.target

# 计算节点
sudo systemctl restart cube-sandbox-compute.target
```

或者发布包目录下：

```bash
sudo ./down.sh
sudo systemctl start cube-sandbox-control.target
```

::: tip 重启 target 等于按依赖顺序重启所有 PartOf 服务
target 本身没有进程，restart target 时 systemd 会顺序重启所有 `PartOf=cube-sandbox-control.target` 的 service。这条命令是替代「逐个写一长串 service 名」的快捷方式。
:::

### 场景 D：完整下线（停止整套服务）

```bash
# 推荐：用发布包内的脚本（自动按角色走）
sudo /root/cube-sandbox-one-click-<version>/down.sh

# 等价命令
sudo systemctl stop cube-sandbox-control.target   # 控制节点
sudo systemctl stop cube-sandbox-compute.target   # 计算节点
```

`down.sh` 不会删除任何数据：MySQL / Redis 数据卷、`/data/cubelet`、`/data/log/...` 等都保留，下次 `start` 即可恢复。

## 查看日志

CubeSandbox 的日志分三类，**不要混淆**：

| 来源 | 包含什么 | 在哪里看 |
|---|---|---|
| **业务行为日志（推荐入口）** | 请求、调度决策、stat、audit、VMM 启动过程 | **`/data/log/<Module>/`** |
| 启动期日志 | systemd 拉起 / hook / ExecStartPost / 退出码 / 容器 build 输出 | `journalctl -u <unit>` |
| 容器内业务日志 | 仅 `cube-proxy`：访问/错误日志写在容器内部 | `docker exec cube-proxy tail /data/log/cube-proxy/error.log` |

### `/data/log/` 业务日志（重点）

> ⚠️ Cubelet / CubeMaster / CubeAPI / network-agent / CubeShim / VMM 的「业务请求 + 统计 + 审计 + VMM 创建过程」日志**全部都写到 `/data/log/`**，**不会**进 journalctl，请直接读文件。

| 模块 | 目录 | 主要文件 |
|---|---|---|
| Cubelet | `/data/log/Cubelet/` | `Cubelet-req.log`（请求）<br>`Cubelet-stat.log`（指标/统计）|
| CubeMaster | `/data/log/CubeMaster/` | `cubemaster-req.log` |
| CubeAPI | `/data/log/CubeAPI/` | `cube-api-YYYY-MM-DD.log`（按天滚动） |
| network-agent | `/data/log/network-agent/` | `network-agent-req.log` |
| CubeShim | `/data/log/CubeShim/` | `cube-shim-req.log`、`cube-shim-stat.log` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` | `vmm.log`（每次创建沙箱都会写 VMM 日志）|
| cube-proxy | 容器内 `/data/log/cube-proxy/` | `error.log`、`access.log`（见下文）|

常用命令：

```bash
# 跟踪 Cubelet 收到的请求
sudo tail -F /data/log/Cubelet/Cubelet-req.log

# 跟踪 CubeAPI（E2B 兼容层）按天滚动的日志
sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log

# 沙箱启动慢 / 启动失败：看 VMM 日志
sudo tail -200 /data/log/CubeVmm/vmm.log
```

### `journalctl` 启动期日志

journalctl 看的是**进程被 systemd 拉起到稳定运行（或失败退出）期间**的 stdout/stderr，主要用于：

- 启动失败的退出码 / 异常信息
- ExecStart / ExecStartPost / ExecStop 各 hook 的输出
- 容器拉镜像、`docker build` / `apk update` 等失败信息
- 服务被自动重启的次数与原因

```bash
# 最近 200 行
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager

# 实时追踪
sudo journalctl -u cube-sandbox-cubemaster.service -f

# 看「上次启动以来」全部
sudo journalctl -u cube-sandbox-cube-api.service -b
```

::: warning journalctl 里没有业务请求日志
进程跑稳之后的 stdout/stderr 输出非常少，因为各组件都把业务日志直接写到 `/data/log/<Module>/`。如果你想看「最近一小时谁创建了沙箱」，**journalctl 是错的入口**，请去 `/data/log/CubeMaster/cubemaster-req.log` 或 `/data/log/Cubelet/Cubelet-req.log`。
:::

### cube-proxy 容器内日志

`cube-proxy` 是基于 OpenResty 的 nginx 容器，访问/错误日志写在容器内的 `/data/log/cube-proxy/`，**不在宿主机文件系统上**，需要 `docker exec` 取：

```bash
sudo docker exec cube-proxy tail -200 /data/log/cube-proxy/error.log
sudo docker exec cube-proxy tail -200 /data/log/cube-proxy/access.log
```

容器名固定为 `cube-proxy`（由 systemd 用 `docker create` 创建）。

### 一键打包诊断信息

如果要拿一整套日志去问社区或提 issue，用内置的诊断收集脚本：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh
```

它会把以下内容统一收集到 `cube-diag-<时间戳>/` 目录下：

- `/data/log/CubeMaster|Cubelet|CubeAPI|CubeShim|CubeVmm|network-agent/` 的 tail
- `cube-proxy` 容器内 `error.log` / `access.log`
- `dmesg` / 进程列表 / 端口 / 挂载 / cgroup / cpuinfo 等环境快照
- 主要配置文件（敏感信息已脱敏）

打包后整体上传：

```bash
tar czf cube-diag-<ts>.tar.gz cube-diag-<ts>/
```

支持选择性收集，例如只取 cubelet + dmesg：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh \
  --module cubelet --module dmesg --lines 500
```

完整选项见 `--help`。

## 常用运维动作速查

| 我想干什么 | 命令 |
|---|---|
| 列出当前角色全部 cube 服务 | `systemctl --no-legend list-units 'cube-sandbox-*'` |
| 查看某个服务状态 | `systemctl status cube-sandbox-<service>.service` |
| 查看某个 target 的依赖图 | `systemctl list-dependencies cube-sandbox-control.target` |
| 启动 / 停止 / 重启单个服务 | `systemctl {start\|stop\|restart} cube-sandbox-<service>.service` |
| 启动 / 停止 / 重启整组 | `systemctl {start\|stop\|restart} cube-sandbox-{control,compute}.target` |
| 看启动失败原因 | `journalctl -u cube-sandbox-<service>.service -n 200 --no-pager` |
| 实时跟踪启动期日志 | `journalctl -u cube-sandbox-<service>.service -f` |
| 重置 failed 计数 | `systemctl reset-failed cube-sandbox-<service>.service` |
| 跑一轮健康检查 | `sudo /root/cube-sandbox-one-click-*/smoke.sh`<br>或 `sudo /usr/local/services/cubetoolbox/scripts/one-click/quickcheck.sh` |
| 完整下线 | `sudo /root/cube-sandbox-one-click-*/down.sh` |
| 收集诊断包 | `sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh` |

## 典型故障排查路径

### 沙箱创建失败 / 超时

按下面的顺序逐层排查：

1. **角色 target 是否 active**

   ```bash
   sudo systemctl status cube-sandbox-control.target
   ```

2. **跑一轮健康检查**

   ```bash
   sudo /root/cube-sandbox-one-click-*/smoke.sh
   ```

3. **CubeAPI 是否收到请求**

   ```bash
   sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log
   ```

4. **CubeMaster 调度链路**

   ```bash
   sudo tail -F /data/log/CubeMaster/cubemaster-req.log
   ```

5. **节点（Cubelet）是否在线、是否收到调度**

   ```bash
   curl http://127.0.0.1:8089/internal/meta/nodes
   sudo tail -F /data/log/Cubelet/Cubelet-req.log
   ```

6. **VMM 启动是否报错**

   ```bash
   sudo tail -200 /data/log/CubeVmm/vmm.log
   ```

### 服务一直 `activating (start-post)` 起不来

```bash
sudo systemctl status cube-sandbox-<service>.service
sudo journalctl -u cube-sandbox-<service>.service -n 200 --no-pager
```

常见根因：

- 容器镜像构建依赖外网（如 `cube-proxy` 的 `apk update`）暂时不可达 → 检查网络，或参阅[部署相关排障](./troubleshooting/deployment.md)
- `ExecStartPost` 健康端口超时（端口被占用 / 上游服务还没起来）
- 对 `cube-sandbox-cube-proxy.service`，`CUBE_PROXY_HTTP_PORT` 是 nginx 实际 HTTP 代理监听端口，也是启动后 TCP 检查使用的端口。`CUBE_PROXY_HOST_PORT` 已废弃且会被忽略；如果需要改检查端口，请改 `CUBE_PROXY_HTTP_PORT`。
- `/data/log` 或 `/data/cubelet` 目录不存在 / 权限不对 / XFS 挂载错位

### Dashboard / API 无法访问

```bash
# WebUI 容器是否在跑
sudo systemctl status cube-sandbox-webui.service
sudo ss -lntp 'sport = :12088'

# CubeAPI 是否在监听
sudo systemctl status cube-sandbox-cube-api.service
sudo ss -lntp 'sport = :3000'
```

## 附录

### 重要路径速查

| 用途 | 路径 |
|---|---|
| 安装目录 | `/usr/local/services/cubetoolbox/` |
| 运行期环境文件 | `/usr/local/services/cubetoolbox/.one-click.env` |
| systemd unit 安装位置 | `/etc/systemd/system/cube-sandbox-*` |
| systemd helper 脚本 | `/usr/local/services/cubetoolbox/scripts/systemd/*.sh` |
| **业务日志（核心入口）** | **`/data/log/<Module>/`** |
| Cubelet 容器层存储（XFS） | `/data/cubelet/` |
| 沙箱镜像 / 快照 | `/data/cube-shim/disks/`、`/data/snapshot_pack/disks/` |
| systemd PID 文件 | `/run/cube-sandbox-systemd/` |

### 角色与服务对照

| 服务 | `control` 节点 | `compute` 节点 |
|---|:-:|:-:|
| `mysql` / `redis` | ✅ | — |
| `cubemaster` | ✅ | — |
| `cube-api` | ✅ | — |
| `webui` | ✅ | — |
| `cube-proxy` / `coredns` / `dns` | ✅ | — |
| `network-agent` | ✅ | ✅ |
| `cubelet` | ✅ | ✅ |

### 相关文档

- [快速开始](./quickstart.md) — 安装入口
- [多机集群部署](./multi-node-deploy.md) — 计算节点的服务子集
- [部署相关排障](./troubleshooting/deployment.md) — XFS / 网段冲突等环境问题
- [模板相关排障](./troubleshooting/templates.md) — 模板创建相关问题
