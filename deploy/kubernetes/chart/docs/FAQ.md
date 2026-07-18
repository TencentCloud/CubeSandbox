# 常见问题 (FAQ)

按主题分类整理 CubeSandbox Helm Chart 部署与运行时的常见问题。若你的问题不在其中,先阅读 [`ARCHITECTURE.md`](ARCHITECTURE.md) 和 [`QUICKSTART.md`](QUICKSTART.md) 排查,再提 issue 时附上 `kubectl -n cube-system get pods -o wide` 与相关 Pod 日志。

- [A. 安装与校验](#a-安装与校验)
- [B. 节点与调度](#b-节点与调度)
- [C. 控制面 / 数据库](#c-控制面--数据库)
- [D. cube-node / PVM 内核 / 沙箱运行时](#d-cube-node--pvm-内核--沙箱运行时)
- [E. CubeProxy / TLS / DNS](#e-cubeproxy--tls--dns)
- [F. Egress 网络](#f-egress-网络)
- [G. 升级、回滚与卸载](#g-升级回滚与卸载)
- [H. 镜像构建](#h-镜像构建)

---

## A. 安装与校验

### A1. `helm install` 报错 `controlPlane.enabled=true requires placement.controlPlane.nodeSelector`

Chart 通过 `templates/validate.yaml` 做启动前校验,禁止用"通配"部署避免误伤节点。补上明确的 nodeSelector:

```yaml
placement:
  controlPlane:
    nodeSelector:
      cube.tencent.com/role: control
```

同样地,以下报错都是校验兜底:

| 报错关键字 | 说明 |
|---|---|
| `cube-node requires placement.compute.nodeSelector` | 计算节点必须显式指定,不能空 |
| `bootstrap.pvmHostKernel.enabled=true requires ... placement.pvm ... allow-pvm-bootstrap` | PVM DaemonSet 的 nodeSelector 必须含该 label；不要写在 `placement.compute` |
| `placement.compute.nodeSelector must not include ... allow-pvm-bootstrap` | 该 label 只属于 `placement.pvm`，写在 compute 会让所有计算节点拉 PVM 镜像 |
| `cubeProxy.enabled=true requires placement.controlPlane.nodeSelector` | CubeProxy 只跑在 control 节点上 |
| `cubeProxy.configureClusterDNS=true requires cubeProxy.domain` | 开启集群 DNS 注入时必须有 sandbox 域名 |

### A2. `helm install --wait` 挂在 CubeNode DaemonSet Ready 数不足

先看三个计算面 DaemonSet 的 desiredNumberScheduled 是否与实际 compute 节点数一致:

```bash
kubectl -n cube-system get ds -l 'app.kubernetes.io/component in (cube-node,cube-node-installer,cube-node-bootstrap,cube-node-pvm)'
# OpenKruise Advanced DaemonSet:
kubectl -n cube-system get ads cube-node
```

- `DESIRED=0` → 没有节点匹配 `placement.compute.nodeSelector`,回到 [QUICKSTART 1.3](QUICKSTART.md#13-打节点-label--taint) 打 label
- `CURRENT<DESIRED` → 至少一台节点被 taint 挡住,检查是否给 compute 节点也加了 toleration
- `READY<CURRENT` → 有 Pod 起来了但没 Ready；Big Pod 未 Ready 时先看 `wait-node-prep` 与 `cube-node-bootstrap`，进入 [D. cube-node](#d-cube-node--pvm-内核--沙箱运行时) 定位

### A3. `helm test` 中 `cube-health-test` 报 CubeAPI /health 失败

大概率是 CubeAPI Pod 未 Ready 或 MySQL 未初始化完:

```bash
kubectl -n cube-system logs deploy/cube-api --tail=200
kubectl -n cube-system logs deploy/cube-master -c cube-master --tail=200
```

- CubeMaster 内嵌 schema migration,大表首次迁移可能耗时数分钟,`--timeout 90m` 是给它留的时间
- 若日志显示"MySQL connection refused",看 [C. 控制面](#c-控制面--数据库)

### A4. `helm test` 挂了没超时也没结果

Helm 3.13+ 才支持 `--timeout` 生效于 test hook。用 `--logs` 直接看进度:

```bash
helm test cube -n cube-system --logs --timeout 20m
```

单独跑失败的测试 Pod:

```bash
kubectl -n cube-system get pods -l app.kubernetes.io/component=test
kubectl -n cube-system logs <test-pod-name>
```

---

## B. 节点与调度

### B1. 我不想给节点打这么多 label,能不能简化?

不建议。Chart 有意采用**显式 label 授权**,原因:

- `pvm-host-bootstrap` 会替换 host kernel 并重启,误伤生产节点后果严重
- `privileged`、hostPath 挂载都要求节点显式授权
- K8s 原生的调度语义,操作者可以精确控制部署范围

生产环境可以通过 GitOps / Terraform 让 label 由基础设施代码统一管理。

### B2. control 节点能否兼做 compute?

技术上可以(单节点试用/小规模);生产不建议。原因:

- Cube Node Big Pod 使用 Pod 网络 + `hostPID` + 特权容器，与控制面争抢节点资源、且升级策略与控制面不同
- MySQL / Redis 与 sandbox 抢 CPU / 内存 / 磁盘 IO,SLA 无法保证

如果确实要合并,给节点同时打两组 label,并**去掉 control taint**,避免 Cube Node 被驱逐。

### B3. PVC / PV 一直 Pending

先确认走的是哪条持久化路径:

```bash
kubectl get sc
kubectl get pvc -n cube-system
kubectl describe pvc -n cube-system <pvc-name>
kubectl -n cube-system describe pod <pod-name>
```

**通用集群(默认不建 SC):**

- 集群没有 default StorageClass → 给 PVC 设 `persistence.storageClassName` 指向已有 SC(如 `local-path` / `gp3`),或改用 hostPath(见 QUICKSTART §6.1 / §6.3)
- 指定的 SC 不存在 / provisioner 未装 → 先装 CSI / local-path-provisioner,再装 chart
- Pod 因 nodeSelector / taint / 资源不足无法调度,且 SC 使用 `WaitForFirstConsumer` → PV 也会一直 Pending;先让 Pod 可调度

**TKE + `values-tke.yaml`(`cube-cbs-wffc`):**

`cube-cbs-wffc` 的 `volumeBindingMode` 是 `WaitForFirstConsumer`,CBS 盘在 Pod 落到具体节点后再创建到该 AZ。常见原因:

- 节点资源不足 → 扩容或降低 request
- Pod 的 nodeSelector 与 CBS 支持的 AZ 不匹配 → 用与 CBS 同 AZ 的节点

### B4. 想临时把 compute 节点摘出去维护

**推荐做法**:先把 Cubelet 上的 sandbox pause 或迁移,再执行:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data=false
# 维护完成后
kubectl uncordon <node>
```

`cube-node` DaemonSet 会在节点重新加入后自动重启并注册到 CubeMaster。

---

## C. 控制面 / 数据库

### C1. cube-master 启动报 "connection refused to mysql"

按顺序检查:

1. `kubectl -n cube-system get pods -l app.kubernetes.io/name=mysql`:内置 MySQL 是否 Running
2. `kubectl -n cube-system get secret cube-secret`:密码是否被误改
3. `kubectl -n cube-system exec cube-mysql-0 -- mysql -uroot -p$(kubectl -n cube-system get secret cube-secret -o jsonpath='{.data.mysql-root-password}' | base64 -d) -e 'show databases'`:内部连通性

外部 MySQL 场景,把 CubeMaster Pod 打到 mysql host 上做连通性验证:

```bash
kubectl -n cube-system exec deploy/cube-master -- \
  sh -c "nc -zv $CUBE_MYSQL_HOST 3306"
```

### C2. 从 v0.4.x 升级到 v0.5.0,MySQL 表结构没升级

CubeMaster v0.5.0 使用内嵌 goose migration,启动时自动跑。若你在同一 chart release 里手工修改过表结构,可能与 embedded migration 冲突。

排查:

```bash
kubectl -n cube-system logs deploy/cube-master -c cube-master | grep -i migrat
# 或直接连数据库查看 goose_db_version 表
```

若 migration lock 卡死(通常是上次 CubeMaster 异常退出),手工释放:

```sql
UPDATE goose_db_version SET is_applied=1 WHERE tstamp = (SELECT MAX(tstamp) FROM goose_db_version WHERE is_applied=0);
```

### C3. 外部 MySQL 8 使用 caching_sha2_password 认证失败

从本 PR 起,chart 已经**移除** `--default-authentication-plugin=mysql_native_password`。CubeMaster / CubeAPI 使用的驱动(`go-sql-driver v1.9`、`sqlx 0.8`)都原生支持 caching_sha2_password。若你的外部 MySQL 用户仍是 old_password,请更新:

```sql
ALTER USER 'cube_user'@'%' IDENTIFIED WITH caching_sha2_password BY '<new-password>';
FLUSH PRIVILEGES;
```

### C4. cubemastercli 交互式登录能否代替 `kubectl exec`?

可以。Chart 交付的 `cubemastercli` Deployment 已经内置一个 bashrc 函数,自动注入 `--address` / `--port`:

```bash
kubectl -n cube-system exec -it deploy/cube-cubemastercli -- bash
# 然后直接输入
cubemastercli node list
cubemastercli sandbox list
```

---

## D. cube-node / PVM 内核 / 沙箱运行时

### D1. `pvm-host-bootstrap` 反复重启 / 节点 reboot

`pvm-host-bootstrap` 跑在 **`cube-node-pvm`** DaemonSet（仅 `placement.pvm` / `allow-pvm-bootstrap` 节点）。绝大多数反复重启是 host kernel 替换后节点需要 reboot。观察:

```bash
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-pvm -c pvm-host-bootstrap --tail=100
kubectl -n cube-system describe pod -l app.kubernetes.io/component=cube-node-pvm
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node-bootstrap -c wait-pvm-host --tail=50
kubectl -n cube-system logs -l app.kubernetes.io/component=cube-node -c wait-node-prep --tail=50
```

正常流程:

1. 首次运行:检测 host kernel → 若不是 PVM kernel,安装 RPM/DEB → **先删 `node-prep-ready` / `pvm-host-ready` / `effective-pvm`** → 触发节点重启
2. 节点重启后:PVM DS 再次运行 → 检测到 PVM kernel（快速路径，**不抢**集群 lease）→ 写带指纹的 `pvm-host-ready`
3. bootstrap `wait-pvm-host` 校验指纹（禁止「文件存在即 ready」）→ 写 `effective-pvm=1` → `cube-node-init` → 写 `node-prep-ready`
4. Big Pod `wait-node-prep` sidecar（Kruise prio 10）校验指纹 Ready → 主容器启动

若卡在 wait-node-prep Not Ready，先查 bootstrap 是否 Ready、`/var/lib/cube-node-bootstrap/node-prep-ready` 是否存在且指纹匹配，再看 wait 容器是否写出 `/run/wait-node-prep.ready`。若卡在 `wait-pvm-host`，查 `cube-node-pvm` 与 `pvm-host-ready` 指纹是否与 live `uname` 一致。

若卡在"waiting for reboot",通常是节点没有 sudo/reboot 权限或 GRUB 未更新,需人工登录节点执行 `reboot` 并检查:

```bash
uname -r     # 应包含 cubesandbox.pvm.host
cat /proc/cmdline | grep -o pti=off
```

### D2. `cube-node-init` 报 `/dev/kvm not accessible`

节点没有 KVM 支持。检查:

```bash
kubectl debug node/<node> -it --image=busybox -- sh
ls -la /dev/kvm
lsmod | grep kvm
```

- 云上 CVM 需要选择支持嵌套虚拟化的实例族(如腾讯云 S5、SA3 及以上带 KVM 的机型)
- 物理机需要 BIOS 开启 VT-x/AMD-V

### D3. cube-node 主容器 Ready 但看不到 sandbox

首先看 CubeMaster 是否收到节点注册:

```bash
kubectl -n cube-system exec deploy/cube-cubemastercli -- \
  sh -c 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
```

- 节点在列表中,`healthy: true` → 正常,可以创建 sandbox
- 节点不在列表 → 看 cube-node 日志

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-node --tail=200 | grep -i register
```

常见问题:CubeMaster endpoint 不可达(参考 [E1](#e1-sandbox-domain-无法解析))。

### D4. sandbox 启动很慢(>10s),但节点 CPU/内存都空闲

- 首次启动:模板 rootfs 需从 CubeMaster storage 下载,单次可能 5-30s(取决于模板大小)。**后续基于同模板的 sandbox**,启动约 1s 以内
- 检查节点 `/data/cubelet` 是否达到 CBS 盘的 IOPS 限制:`iostat -x 1`
- 检查是否有 sandbox 处于 Paused 状态占用磁盘但未及时清理

### D5. Cube sandbox 数量瓶颈

单节点承载 sandbox 数量的三个瓶颈:

1. **内存**:每 sandbox base 256MB-2GB,active 数受限于节点内存
2. **磁盘**:模板 CoW 后每 sandbox rootfs 约几百 MB,受限于 `/data/cubelet` 盘容量
3. **KVM 虚拟机数**:Linux 单节点 KVM 支持最多几百个 VM,内核参数决定

利用 `Pause` 可以把非活跃 sandbox 归零(CPU/RSS→0),盘不释放。运营层面通常在闲置 N 分钟后 Pause,更长后 Destroy。

---

## E. CubeProxy / TLS / DNS

### E1. sandbox domain 无法解析

按方向排查:

1. **集群内**（Pod / 跟随节点 DNS 的 sandbox guest）:`nslookup test.cube.app`
   - 无应答 → 查 `kubectl -n kube-system get cm coredns -o yaml` 是否含 `# BEGIN cube-sandbox-dns`
   - 或看 Job：`kubectl -n cube-system logs job/cube-cluster-dns-apply`
   - 应答应是 CubeProxy **ClusterIP**；对照 `kubectl -n cube-system get svc cube-proxy -o wide`
2. **guest DNS**：默认 `followNodeDns=true`；显式覆盖用 `cubeNode.dns.sandbox.nameservers`
3. **集群外部**(客户浏览器 / SDK):把 `cube.app` / `*.cube.app` 指到 Ingress Controller / LB

### E2. Ingress / 外部入口不通

CubeProxy 不再占用节点 80/443。排查:

- Ingress Controller 是否存在：`kubectl get ingressclass`、`kubectl get ingress -n cube-system`
- passthrough 注解是否匹配你的 Controller（默认是 nginx-ingress）
- TKE：`-f values-tke.yaml` 默认 **关闭 Ingress**，改用 `LoadBalancer` Service（CLB）暴露 CubeProxy，DNS 指到 Service EXTERNAL-IP；不要用 NodePort 硬接 qcloud Ingress
- nginx Controller 是否开启 SSL passthrough（需 `--enable-ssl-passthrough`）
- 无 Ingress 时也可设 `cubeProxy.ingress.enabled=false`，自行把流量接到 Service

### E3. selfSigned TLS 浏览器提示不安全

selfSigned 只用于试用。生产环境请使用:

- **existingSecret**(推荐):
  ```yaml
  cubeProxy:
    tls:
      mode: existingSecret
      existingSecret: my-cube-tls
  ```
  提前 `kubectl create secret tls my-cube-tls --cert=... --key=...`
- **certManager**:
  ```yaml
  cubeProxy:
    tls:
      mode: certManager
      certManager:
        issuerRef:
          name: my-letsencrypt-issuer
          kind: ClusterIssuer
  ```

### E4. 修改 `cubeProxy.advertiseIP` 后未生效

CubeProxy 的路由信息来自 Redis(sandbox owner 表),chart 只修改**入口 IP 广播**。sandbox 存量数据不会自动改写,新建的 sandbox 会用新 advertiseIP。

如需强制刷新:

```bash
kubectl -n cube-system rollout restart deployment/cube-proxy
```

---

## F. Egress 网络

### F1. sandbox 无法访问外网

egress sidecar 采用**默认拒绝**策略,只允许白名单域名。检查:

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress --tail=200 | grep -i deny
```

放开某个域名:调整 `cubeEgress` values 或运行时通过 CubeMaster API 更新白名单。生产环境建议按需最小化。

### F2. sandbox 出站被 MITM 后 TLS 报错

egress 使用自签 CA 做 MITM。sandbox 内的应用需要信任 chart 生成的 CA:

```bash
# 从 Kubernetes 拿 CA 证书
kubectl -n cube-system get secret cube-egress-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > cube-ca.crt
# 注入 sandbox 模板中(如 /etc/ssl/certs/)
```

或者用 `cubeEgress.ca.mode: existingSecret` 复用企业 CA。

### F3. cube-egress-net sidecar 反复 CrashLoopBackOff

`cube-egress-net` 依赖 `cube-dev` 网口(cube-node 主容器创建)。启动顺序问题:

```bash
kubectl -n cube-system logs <cube-node-pod> -c cube-egress-net --tail=100
```

- 若卡在 "waiting for cube-dev" → 主容器还没起,通常自愈,若持续超过 5 分钟检查主容器日志
- 若卡在 "TPROXY rule failed" → 节点上 iptables 版本过旧,需 iptables-nft;或与其他 CNI(如 Calico)规则冲突

---

## G. 升级、回滚与卸载

### G1. `helm upgrade` 后 cube-node 升级,会不会中断存量沙箱? Pod IP 会变吗?

**默认不会中断存量沙箱**；Big Pod 使用 OpenKruise Advanced DaemonSet 的 **InPlaceIfPossible**，仅换镜像时 **Pod UID / IP / 网络命名空间不变**。

机制对齐一键包「停服务、不杀 shim、覆盖二进制、重启」：

1. **`cube-node-installer`** 与 Big Pod **self-stage** 从 `/opt/cube-image` stage 到 `/usr/local/services/cubetoolbox`（整树 hostPath；原子目录替换）。升产物只动 Installer；升控面只动 Big Pod。**`cube-node-pvm`** 负责 PVM host；**`cube-node-bootstrap`** 负责 wait-pvm / node-init 并写 `node-prep-ready`。分工见 [`ARCHITECTURE.md`](ARCHITECTURE.md)；步骤见 [`UPGRADE.md`](UPGRADE.md)。
2. shim ttrpc / VMM socket 目录也是 hostPath：容器 `/run/containerd` → 宿主机 `/data/cubelet/run/containerd`，容器 `/run/vc` → 宿主机 `/data/cubelet/run/vc`。
3. `/data/cubelet/state` 通过启动前 `mount --bind` 留在 hostPath 上。
4. `preStop` / entrypoint 只 TERM **cubelet** 与 **network-agent**，不匹配 `containerd-shim-cube-rs` / `cube-runtime`。
5. 原地升级后 cubelet `LoadExistingShims` + `RecoverAllCubebox`、network-agent `recover()` 重连。
6. 默认 `rollingUpdateType: InPlaceIfPossible`，`maxUnavailable: 1`；readiness 在 recover 完成后才 Ready。

**例外（会 recreate）**：增删 Big Pod 容器（含首次引入 `cube-slot-1`…`6`）、改 volumeMount / securityContext / 容器名 / 直接改 env。占位槽服务名应改 Pod annotation `cube.tencent.com/slot-N`（values：`cubeNode.placeholderSlots.services`），不要改容器名。

完整步骤见 [`UPGRADE.md`](UPGRADE.md)。集群需已装 OpenKruise（安装命令见 [`QUICKSTART.md`](QUICKSTART.md) §1.4）。

若从仍使用 `cube-proxy-node` 资源名的旧 Chart 升级：Deployment/Service 现为 `cube-proxy`，Helm 会删旧建新（Proxy 短暂中断一次），见 [`UPGRADE.md`](UPGRADE.md)。

若仍希望完全手工控制节奏：

```yaml
cubeNode:
  updateStrategy:
    type: OnDelete
```

```bash
kubectl -n cube-system delete pod -l app.kubernetes.io/component=cube-node --field-selector spec.nodeName=<node>
```

**注意**：

- 升级的是控制组件镜像；存量沙箱仍跑**旧** shim/runtime 二进制（N/N-1），直到沙箱自然销毁。
- **加新产物组件**：只改 Installer 模板，不要往 Big Pod 加容器。
- 不要在升级路径执行 `cubecli unsafe init` / `InitHost`，那会 Destroy 全部沙箱。
- 若改 Big Pod YAML 中不可原地字段（直接改 env/volumeMounts / 增删容器）或 `rollingUpdateType: Standard`，会退化为重建，IP 通常会变。
- 升级窗口内**新建**沙箱可能短暂失败并被 CubeMaster reschedule，见 UPGRADE.md。

### G2. `helm rollback` 会回滚 host kernel 吗?

**不会**。Helm 只管 K8s 资源。Host kernel、GRUB、`/etc/fstab`、XFS 挂载点等由 `pvm-host-bootstrap` 做的节点级修改,rollback **完全不动**。生产环境请提前准备 host kernel 回滚 runbook(RPM 降级、切换 GRUB 默认项、reboot 验证)。

### G3. `helm uninstall` 后节点上 sandbox 数据还在

设计如此。卸载**只删 K8s 资源**,以下需人工清理:

```bash
# 在每个 compute 节点执行
sudo rm -rf /data/cubelet /data/cube-shim /data/snapshot_pack /data/log /usr/local/services/cubetoolbox /tmp/cube
# 如果不再需要 PVM host kernel,还需要:
# 1. 卸载 kernel 包
# 2. 更新 GRUB 默认引导
# 3. reboot
```

CubeMaster / MySQL / Redis 的 PVC 遵循**实际绑定的** StorageClass 的 `reclaimPolicy`（通用路径下通常是集群 default SC 的策略）。仅当使用 chart 创建的 SC（例如 `values-tke.yaml` 的 `cube-cbs-wffc`，`storageClass.reclaimPolicy: Delete`）时，才由 chart 侧默认 `Delete`。若 policy=`Retain`，PVC/PV 会保留。

---

## H. 镜像构建

### H1. `build-cube-images.sh` 下载 tarball 卡住 / 失败

- 检查网络:`curl -I https://downloads.sourceforge.net/`
- 手动预下载并跳过脚本下载:
  ```bash
  mkdir -p /tmp/cube-kubernetes-images-v0.5.1/downloads
  cd /tmp/cube-kubernetes-images-v0.5.1/downloads
  # 从 SourceForge 手动下载 3 个文件
  # 然后重新运行 build-cube-images.sh,会检测到本地文件跳过下载
  ```

### H2. 构建 arm64 镜像

```bash
ONE_CLICK_ARCH=arm64 \
PUSH=1 REGISTRY=<your-registry> IMAGE_TAG=v0.5.1-arm64 \
./deploy/kubernetes/images/build-cube-images.sh
```

需要 arm64 build machine 或者 buildx multi-arch。

### H3. 想只重构某个镜像(如 cube-api),不重跑整个流水线

脚本目前无子命令拆分,但可以设置环境变量跳过其他步骤,或复用中间产物:

- `PACKAGE_DIR_OVERRIDE=/tmp/cube-kubernetes-images-v0.5.1/sandbox-package` → 跳过下载 / 解压
- 手工 `docker build` 到需要的镜像

后续版本考虑加子命令。

### H4. 老版本 curl (`<7.71`) 报 `option --retry-all-errors: is unknown`

已在本 PR 修复。脚本会在运行时探测 curl 是否支持该 flag,不支持则自动跳过。若你自己 fork 了老版本脚本,直接把 `--retry-all-errors` 那行删了就行。

---

## 提问模板

如果以上都没覆盖你的问题,提 issue 时请附上:

```text
Chart 版本: v0.5.1
K8s 版本: (kubectl version --short 输出)
运行环境: (TKE / 自建 / 单节点 / 多可用区)
values.yaml 关键片段(隐去密码)
Failing 组件: (Pod 名 + `kubectl describe` + `kubectl logs`)
```
