# 常见问题

按主题整理 Helm Chart 部署与运行中的高频问题。先对照本节；仍无法解决时再看 [Helm 安装](./install.md)、[架构说明](./architecture.md)、[升级](./upgrade.md)。提 Issue 时请附上文末[提问模板](#提问模板)。

::: tip 命令里的资源名
下文示例默认 Release 名为 `cube`、命名空间为 `cube-system`（资源名形如 `cube-master`、`cube-secret`）。若 Release 名不同，请换成 `<release>-…`，或改用 `app.kubernetes.io/component=…` 标签选择器（不依赖 Release 名）。
:::

## 目录

- [安装与校验](#安装与校验)
- [节点与调度](#节点与调度)
- [控制面 / 数据库](#控制面--数据库)
- [计算面 / PVM / 沙箱](#计算面--pvm--沙箱)
- [CubeProxy / TLS / DNS](#cubeproxy--tls--dns)
- [Egress 网络](#egress-网络)
- [升级、回滚与卸载](#升级回滚与卸载)
- [镜像构建](#镜像构建)

---

## 安装与校验

### `helm install` 报错要配置 `placement.*.nodeSelector`

Chart 用 `templates/validate.yaml` 禁止「通配」部署，避免误伤节点。补上明确的 nodeSelector，例如：

```yaml
placement:
  controlPlane:
    nodeSelector:
      cube.tencent.com/cube-control: "true"
```

同类校验：

| 报错关键字 | 含义 |
| --- | --- |
| `cube-node requires placement.compute.nodeSelector` | 计算节点必须显式指定 |
| `…placement.pvm… allow-pvm-bootstrap` | PVM DaemonSet 的 selector 必须含该 label；不要写进 `placement.compute` |
| `placement.compute… must not include …allow-pvm-bootstrap` | 写进 compute 会让所有计算节点拉 PVM 大镜像 |
| `cubeProxy.enabled=true requires placement.controlPlane.nodeSelector` | Proxy 跑在控制面节点上 |
| `configureClusterDNS=true requires cubeProxy.domain` | 注入集群 DNS 时必须有 sandbox 域名 |

节点怎么打标签见 [Helm 安装 · 打标签](./install.md#3-给节点打标签及角色污点)。

### 计算面 DaemonSet Ready 数不够

先看四条原生 DaemonSet：

```bash
kubectl -n cube-system get daemonset \
  -l 'app.kubernetes.io/component in (cube-node,cube-node-installer,cube-node-bootstrap,cube-node-pvm)'
```

| 现象 | 处理 |
| --- | --- |
| `DESIRED=0` | 没有节点匹配 `placement.compute` → 补 `cube-node=true` label |
| `CURRENT < DESIRED` | 污点挡住了 → 检查 `compute` 污点与 toleration |
| `READY < CURRENT` | Pod 起来了但未 Ready → Big Pod 先看 `wait-node-prep` 与 bootstrap，见[计算面](#计算面--pvm--沙箱) |

### `helm test` 里 CubeAPI `/health` 失败

多数是 CubeAPI 未 Ready，或 MySQL / migration 未完成：

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=api --tail=200
kubectl -n cube-system logs -l app.kubernetes.io/component=master -c cube-master --tail=200
```

- CubeMaster 内嵌 schema migration，首次可能要几分钟；安装时用 `--timeout 90m`
- 日志若是 MySQL connection refused → 见[控制面 / 数据库](#控制面--数据库)

### `helm test` 卡住、既不超时也没结果

Helm **3.13+** 才让 `--timeout` 对 test hook 生效。加上 `--logs`：

```bash
helm test cube -n cube-system --logs --timeout 20m
```

单独看失败的测试 Pod：

```bash
kubectl -n cube-system get pods -l app.kubernetes.io/component=test
kubectl -n cube-system logs <test-pod-name>
```

---

## 节点与调度

### 能不能少打几枚 label？

不建议。Chart 用显式 label 授权，是因为：

- `pvm-host-bootstrap` 会换 host kernel 并可能 reboot
- 计算面有 privileged / hostPath，误调度代价大

生产可用 GitOps / Terraform 统一管 label。

### 控制面节点能否兼做计算节点？

技术上可以（单节点试用）；生产不建议——资源争抢、升级策略也不同。

混部时：

1. 同一节点打两枚独立 label：`cube-control=true` 与 `cube-node=true`（不要用同一个 key 覆盖写）
2. 叠加 `values-single-node.yaml`，让控制面 / 计算面都容忍两把角色污点
3. 需要 PVM 时再加 `allow-pvm-bootstrap=true`

扩容纯计算节点时只打 `cube-node` + `compute` 污点，**不要**打 `cube-control`。步骤见 [Helm 安装 · 单节点](./install.md#42-单节点试用一台机器既做控制面又做计算面)。

### PVC / PV 一直 Pending

```bash
kubectl get sc
kubectl get pvc -n cube-system
kubectl describe pvc -n cube-system <pvc-name>
kubectl -n cube-system describe pod <pod-name>
```

**通用集群（默认不建 SC）：**

- 没有 default StorageClass → 设 `persistence.storageClassName`，或改用 hostPath（见[安装 · 准备配置](./install.md#5-准备配置文件)）
- 指定的 SC / CSI 不存在 → 先装 provisioner
- SC 是 `WaitForFirstConsumer`，但 Pod 因 selector / taint 调度不上 → 先让 Pod 可调度

**TKE + `values-tke.yaml`：** CBS 盘在 Pod 落点后再创建；常见是节点资源不足，或 nodeSelector 与 CBS 可用区不一致。

### 临时摘掉一台 compute 做维护

先 销毁节点上的沙箱，再：

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data=false
# 维护完成后
kubectl uncordon <node>
```

`cube-node` 会在节点重新可调度后自动起来并重新注册。

---

## 控制面 / 数据库

### cube-master 报连不上 MySQL

按顺序查（以下命令假定 Release 名为 `cube`，命名空间 `cube-system`；其它 Release 名请把资源名前缀换成 `<release>`）：

1. 内置 MySQL 是否 Running：`kubectl -n cube-system get pods -l app.kubernetes.io/component=mysql`
2. Secret 密码是否被误改：`kubectl -n cube-system get secret cube-secret`
3. 容器内连通性：

```bash
kubectl -n cube-system exec cube-mysql-0 -- mysql -uroot \
  -p"$(kubectl -n cube-system get secret cube-secret -o jsonpath='{.data.mysql-root-password}' | base64 -d)" \
  -e 'show databases'
```

外部 MySQL（在 **CubeAPI** Pod 上探测；环境变量是 `CUBE_SANDBOX_MYSQL_*`，不是 `CUBE_MYSQL_HOST`）：

```bash
kubectl -n cube-system exec -l app.kubernetes.io/component=api -- \
  sh -c 'nc -zv "$CUBE_SANDBOX_MYSQL_HOST" "$CUBE_SANDBOX_MYSQL_PORT"'
```

CubeMaster 侧 MySQL 地址写在挂载的 `conf.yaml`（由 Chart 渲染），不通过上述 env 注入。

### 升级后表结构没迁完 / migration 卡住

CubeMaster 启动时跑内嵌 goose migration。查日志：

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=master -c cube-master | grep -i migrat
```

也可连库看 `goose_db_version`。若上次异常退出导致 lock 卡住，需按库内状态手工处理（谨慎操作）。

### 外部 MySQL 8 `caching_sha2_password` 认证失败

Chart 已不再强制 `mysql_native_password`。CubeMaster / CubeAPI 驱动支持 `caching_sha2_password`。若用户仍是旧插件：

```sql
ALTER USER 'cube_user'@'%' IDENTIFIED WITH caching_sha2_password BY '<new-password>';
FLUSH PRIVILEGES;
```

### 怎么用 cubemastercli？

```bash
# 进入交互 shell（镜像内 bashrc 会自动补 --address / --port）
kubectl -n cube-system exec -it -l app.kubernetes.io/component=cubemastercli -- bash
cubemastercli node list
cubemastercli sandbox list
```

也可一行执行（与 Chart `NOTES.txt` 一致）：

```bash
kubectl -n cube-system exec deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
```

---

## 计算面 / PVM / 沙箱

### `pvm-host-bootstrap` 反复重启 / 节点 reboot

跑在 **`cube-node-pvm`**（仅 `allow-pvm-bootstrap` 节点）。多数「反复重启」其实是换核后的正常 reboot。先看日志：

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-pvm -c pvm-host-bootstrap --tail=100
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-bootstrap -c wait-pvm-host --tail=50
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node -c wait-node-prep --tail=50
```

正常链路（摘要）：ensure `pvm-not-ready` → 腾空依赖 Pod → invalidate ready 标记 → 换核 / reboot → 写 `pvm-host-ready` 并清闩 → bootstrap 写 `node-prep-ready` → Big Pod `wait-node-prep` init 退出 → 主容器启动。完整说明见[架构说明 · PVM](./architecture.md#pvmcube-node-pvm)。

若卡在 wait-node-prep：先确认 bootstrap Ready，以及 `/var/lib/cube-node-bootstrap/node-prep-ready` 指纹。若换核后迟迟不重启，多半是节点无 reboot 权限或 GRUB 未更新——登录节点人工 `reboot`，并检查：

```bash
uname -r     # 应含 cubesandbox.pvm.host（与 Chart 默认 DESIRED_KERNEL_PATTERN 一致）
cat /proc/cmdline | grep -o pti=off
```

### `cube-node-init` 报 `/dev/kvm` 不可用

```bash
kubectl debug node/<node> -it --image=busybox -- sh
ls -la /dev/kvm
lsmod | grep kvm
```

云主机需安装PVM；物理机需开 VT-x / AMD-V。

### cube-node Ready 了，但看不到沙箱 / 节点没注册

```bash
kubectl -n cube-system exec -l app.kubernetes.io/component=cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
```

- `healthy: true` → 可创建沙箱
- 不在列表 → 看注册日志：

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node -c cubelet --tail=200 | grep -i register
```

常见原因是连不上 CubeMaster（网络 / DNS）。

### 沙箱启动很慢（>10s），节点却很空

- **首次**用某模板：要从控制面拉 rootfs，可能 5–30s；同模板后续通常约 1s 内
- 查 `/data/cubelet` 是否打满盘或 IOPS：`iostat -x 1`
- 是否有大量 Paused 沙箱占盘未清理

### 单节点沙箱数量大概卡在哪？

1. **内存**：每沙箱约数百 MB～数 GB
2. **磁盘**：CoW rootfs，看 `/data/cubelet` 容量（默认 loopback 仅 **25G**，见[安装 · 计算节点数据盘](./install.md#计算节点数据盘datacubelet)）
3. **KVM 数量**：单机通常最多数百级，受内核参数影响

`Pause` 可把非活跃沙箱的 CPU/RSS 压到接近 0（盘不释放）。运营上常：闲置 N 分钟 Pause，更久 Destroy。

### `/data/cubelet` 空间不够 / 想改数据盘大小

默认 `bootstrap.nodeInit.dataCubelet.loopback.size` 为 `25G`，bootstrap 首次会创建 `/data/cubelet-xfs.img` 并挂到 `/data/cubelet`。

- **尚未装过 / 镜像文件还不存在**：在 values 里改 `size`（如 `200G`）后重装或重跑 bootstrap 即可。
- **镜像已经建好**：再改 values **不会**自动扩容。生产建议关掉 loopback，改用预挂载的大容量 XFS 盘；若必须重建 loopback，需维护窗口删旧 img（会清空该路径数据）。

配置示例见 [Helm 安装 · 计算节点数据盘](./install.md#计算节点数据盘datacubelet)。

### 怎么关掉某台节点的 PVM？

**去掉该节点的 label**（不要只改 Chart 默认开关）：

```bash
kubectl label node <node> cube.tencent.com/allow-pvm-bootstrap-
```

`cubeNode.pvmGuestKernel.enabled=false` 主要影响**首次安装默认**；已在跑 PVM 的节点升级时会尽量保持原样。

整集群不做 PVM 换核：不要打 `allow-pvm-bootstrap`，并可设 `bootstrap.pvmHostKernel.enabled=false`。

自检当前 guest：

```bash
readlink /usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux
# 或
readlink /var/lib/cube-node-bootstrap/vmlinux-active
```

---

## CubeProxy / TLS / DNS

### sandbox 域名解析不了

1. **集群内**（含 follow 节点 DNS 的 guest）：`nslookup test.cube.app`
   - 无应答 → 看 CoreDNS 是否含 `# BEGIN cube-sandbox-dns`（后跟 Release 名），或 Job 日志：`kubectl -n cube-system logs job/cube-cluster-dns-apply`
   - 应答应是 CubeProxy ClusterIP：`kubectl -n cube-system get svc cube-proxy -o wide`
2. **guest DNS**：默认 `cubeNode.dns.sandbox.followNodeDns=true`；可显式设 `cubeNode.dns.sandbox.nameservers`
3. **集群外**：把 `cube.app` / `*.cube.app` 指到 Ingress / LB

### Ingress / 外部入口不通

CubeProxy **不再**占节点 80/443。排查：

- IngressClass / Ingress 是否存在
- passthrough 注解是否匹配你的 Controller（默认按 nginx-ingress）
- TKE：`values-tke.yaml` 默认关 Ingress，用 LoadBalancer（CLB）；DNS 指 EXTERNAL-IP
- 无 Ingress：设 `cubeProxy.ingress.enabled=false`，自行接到 Service

### selfSigned 证书浏览器告警

试用预期行为。生产用：

```yaml
cubeProxy:
  tls:
    mode: existingSecret   # 或 certManager
    existingSecret: my-cube-tls
```

### 改了 `cubeProxy.advertiseIP` 好像没生效

`cubeProxy.advertiseIP` 在 Chart 里只是**给人看的提示字段**（`values.yaml` 注释写明 optional hint），**不会**写入 Proxy / DNS / Service 模板，改它再重启 Proxy 也不会改变集群行为。

真正要改的是：

- 外部 DNS / LB 指向的地址
- 或 `cubeProxy.service` / Ingress 相关配置

若只想滚动 Proxy 副本：

```bash
kubectl -n cube-system rollout restart deploy/cube-proxy
```

---

## Egress 网络

### 沙箱上不了外网

egress 默认拒绝，只放行白名单：

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress --tail=200 | grep -i deny
```

按需改 `cubeEgress` values，或经 CubeMaster API 更新白名单。

### 出站 MITM 后 TLS 报错

需要信任 Chart 下发的 egress CA：

```bash
kubectl -n cube-system get secret cube-egress-ca \
  -o jsonpath='{.data.cube-root-ca\.crt}' | base64 -d > cube-ca.crt
# 注入模板 / guest 信任链，例如 /etc/ssl/certs/
```

（Secret 名默认 `<release>-egress-ca`；证书 key 默认是 `cube-root-ca.crt`，不是 `ca.crt`。）

或用 `cubeEgress.ca.mode: existingSecret` 复用企业 CA。

### `cube-egress-net` CrashLoopBackOff

它依赖主容器创建的 `cube-dev`：

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress-net --tail=100
```

- 日志出现 `interface cube-dev not present` → **network-agent / cubelet** 尚未创建 `cube-dev`，通常会自愈；超过约 5 分钟查这两类容器日志
- 规则反复 `rule reapply failed` → iptables 过旧（需 nft）或与 CNI 规则冲突

---

## 升级、回滚与卸载

### `helm upgrade` 会不会中断存量沙箱？Pod IP 会变吗？

**升 Big Pod 运行时镜像 / 改 Pod template：会。** `cube-node` 是原生 DaemonSet，变更会 recreate Pod（UID / IP / netns 变化），该节点存量沙箱会中断。只升 Installer / Bootstrap / PVM 且不动 Big Pod template 时，Big Pod 可保持不变。步骤与红线见 [升级](./upgrade.md)。

会 recreate Big Pod 的典型操作：bump `images.cubelet` 等运行时镜像、增删容器、改 volumeMount / securityContext / 容器名 / env。

### `helm rollback` 会回滚 host kernel 吗？

**不会。** Helm 只管 K8s 对象。kernel / GRUB / fstab / XFS 等需单独的宿主机回滚 runbook。

### `helm uninstall` 后节点上数据还在

设计如此。卸载只删 Chart 管理的对象。计算节点上常见残留：

```bash
sudo rm -rf /data/cubelet /data/cube-shim /data/cube-shared /data/snapshot_pack \
  /data/log/Cubelet /data/log/CubeShim /data/log/CubeVmm \
  /usr/local/services/cubetoolbox /var/lib/cube-node-bootstrap /tmp/cube \
  /data/cubelet-xfs.img
```

PVM 内核还需卸包、改 GRUB、reboot。也可用 Chart 附带的 `deploy/kubernetes/chart/scripts/cleanup-node-host.sh`。

PVC/PV 是否删除取决于实际 StorageClass 的 `reclaimPolicy`（TKE 的 `cube-cbs-wffc` 常见为 `Delete`；`Retain` 则会留下）。

---

## 镜像构建

### `build-cube-images.sh` 下载卡住

- 默认从 **GitHub Releases** 拉一体化包；国内可设 `MIRROR=cn` 走 CNB（不是 SourceForge）
- 或预下载到 `${BUILD_ROOT}/downloads/`（默认 `BUILD_ROOT=/tmp/cube-kubernetes-images-<version>`），脚本检测到本地文件会跳过下载

见 [`deploy/kubernetes/images/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/images/README.md)。

### 构建 arm64

```bash
ONE_CLICK_ARCH=arm64 \
PUSH=1 REGISTRY=<your-registry> IMAGE_TAG=v0.6.0-rc3 \
./deploy/kubernetes/images/build-cube-images.sh
```

需要 arm64 机器或 buildx multi-arch。

### 只想重打某一个镜像

可以。给脚本传镜像名即可（不需要的下载 / `SOURCE_REF` 导出会按需跳过）：

```bash
./deploy/kubernetes/images/build-cube-images.sh cubelet
./deploy/kubernetes/images/build-cube-images.sh cubelet cube-shim
```

完整列表见 `./deploy/kubernetes/images/build-cube-images.sh --help`，说明见 [`images/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/kubernetes/images/README.md)。

若发布包已解压好，可用 `PACKAGE_DIR_OVERRIDE` 指向该目录，避免重复下载。本地开发还可 `LOCAL_BIN=1` / `--local` 把 `_output/bin` 叠进镜像。

### 老 curl 报 `--retry-all-errors` 未知

当前脚本**默认不会**加 `--retry-all-errors`；仅当显式设置 `CURL_RETRY_ALL_ERRORS=1` 且本机 curl 支持该 flag 时才会启用。一般无需关心；自改的旧 fork 若硬编码了该参数，删掉或升级 curl 即可。

---

## 提问模板

```text
Chart 版本:
K8s 版本: (kubectl version)
运行环境: (TKE / 自建 / k3s / 单节点 …)
关键 values 片段: (隐去密码)
失败组件: Pod 名 + kubectl describe + kubectl logs
kubectl -n cube-system get pods -o wide
```

---

## 下一步

- [Helm 安装](./install.md)
- [架构说明](./architecture.md)
- [升级](./upgrade.md)
