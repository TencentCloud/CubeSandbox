# CubeSandbox 0.6 K8s 部署记录（64_host）

> 部署日期：2026-08-03
> 部署方式：Helm Chart（Kubernetes 交付）
> 控制面：`63_host`（k8s-master，192.168.4.63）
> 计算面：`64_host`（k8s-node1，192.168.4.64，PVM 虚拟化）
> 最终结果：`helm test` 8/8 通过，`cube` release v13 deployed；沙箱访问验证 `verify_cubesandbox.py` 34/34 通过

> ::: warning 敏感信息
> 本文档包含部署时生成的凭据（MySQL/Redis 密码、CubeProxy admin token），仅限内部部署记录使用，**请勿提交到公开仓库**。
> :::

> **目录**：一 背景与目标 · 二 环境信息 · 三 前置检查 · 四 部署前准备 · 五 部署时间线 · 六 关键问题（helm 部署） · 七 helm test 验证 · 八 Chart 修改 · 九 访问方式 · 十 注意事项 · **十一 沙箱访问验证与排障（2026-08-03 晚，含 MTU 黑洞与 auto_resume 修复）**

---

## 一、背景与目标

在现有 K8s 集群上以 Helm Chart 方式部署 CubeSandbox 0.6：

- `64_host` 无硬件虚拟化（VMware 未开启嵌套虚拟化，无 `/dev/kvm`），沙箱运行路径选择 **PVM（Pagetable-based Virtual Machine，软件虚拟化）**。
- `64_host` 上已有 EMQX、Node-RED、gitlab-runner×3 等负载，已确认接受 PVM 安装触发节点重启造成的短暂中断。
- `63_host` 为集群主节点（control-plane），复用为 CubeSandbox 控制面部署位置。

## 二、环境信息

| 项 | 值 |
|---|---|
| 集群版本 | Kubernetes v1.30.6 |
| k8s-master | 192.168.4.63，Ubuntu 20.04.6 LTS，containerd 1.7.24 |
| k8s-node1（64_host） | 192.168.4.64，Ubuntu 20.04.6 LTS，containerd 1.7.23 |
| k8s-librax4 | 192.168.4.68，NotReady，未打 cube-node 标签，不参与 |
| 沙箱虚拟化 | PVM（部署后 64_host 内核变为 `6.6.69-opencloudos9.cubesandbox.pvm.host-g0de43d6b3bcd`） |
| Cubevs CIDR | `172.16.0.0/18`（preflight 校验通过） |
| 持久化 | hostPath（集群无默认 StorageClass） |

### 节点标签

```bash
kubectl label node k8s-node1 cube.tencent.com/cube-node=true
kubectl label node k8s-node1 cube.tencent.com/allow-pvm-bootstrap=true   # 触发 PVM 内核安装
```

## 三、前置检查与结论

| 检查项 | 结果 | 处理 |
|---|---|---|
| `/dev/kvm` / CPU `vmx/svm` | 无（VMware 未开嵌套虚拟化） | 使用 PVM |
| cgroup v2 | 非必需；Cubelet 自动适配 v1/v2（`cgroup/local.go`） | 无需处理 |
| 默认 StorageClass | 无（仅有 doris-be/doris-fe 的 no-provisioner） | 控制面组件改用 hostPath |
| `kubectl` 权限（63_host root） | 需 `KUBECONFIG=/etc/kubernetes/admin.conf` | 后续命令均显式指定 |
| XFS + loopback | 满足 | 无需处理 |
| glibc / Docker | 满足 | 无需处理 |

## 四、部署前准备

### 4.1 工作目录与 chart 同步

```bash
# 在 63_host 上准备目录，从开发机同步 chart 源码
mkdir -p /tmp/cube-deploy
tar -czf /tmp/cube-deploy/chart.tgz -C /workspace/github/CubeSandbox deploy/kubernetes/chart
scp -o StrictHostKeyChecking=no /tmp/cube-deploy/chart.tgz 63_host:/tmp/cube-deploy/
ssh 63_host 'cd /tmp/cube-deploy && tar -xzf chart.tgz --overwrite'
```

### 4.2 runtime-values.yaml

```yaml
cubeProxy:
  advertiseIP: "192.168.4.63"        # 控制面所在节点 IP
  domain: "cube.app"
  tls:
    mode: selfSigned                  # 无现成证书，使用自签

mysql:
  host: ""
  password: "58e5b968327c23458bc64d8f0b8565bf"
  rootPassword: "bb2febc29e7c40574ea088059af5dd3d"
  persistence:
    hostPath: /data/mysql             # 无默认 StorageClass，改用 hostPath

redis:
  host: ""
  password: "55168c39f049ba3bddc025ff9312e3ff"
  persistence:
    hostPath: /data/redis

controlPlane:
  master:
    persistence:
      hostPath: /data/CubeMaster/storage

# k8s-master 有 node-role.kubernetes.io/control-plane:NoSchedule 污点，
# 控制面组件需额外容忍该污点才能调度到控制面节点
placement:
  controlPlane:
    tolerations:
      - key: cube.tencent.com/control
        operator: Equal
        value: "true"
        effect: NoSchedule
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule

# 64_host 是 Ubuntu（Debian 系），但 bootstrap 脚本优先尝试 rpm 包导致失败。
# 将 rpmPath 指向不存在的路径，强制走 deb 分支安装 PVM 内核。
bootstrap:
  pvmHostKernel:
    package:
      rpmPath: /artifacts/kernel-pvm-host.rpm.unused
```

> 注意：`mysql` / `redis` 的 `persistence` 必须合并到同一 key 下，否则会覆盖前面已生成的密码（见「问题 5」）。

## 五、部署执行时间线

| 时间 | 动作 | 结果 |
|---|---|---|
| 16:08–16:10 | 生成 runtime-values.yaml，`helm template` 渲染校验 | 通过（render.out 15 万字节） |
| 16:16 | 首次 `helm install cube` | 失败：`cubevs-cidr-preflight` 预检 Job `DeadlineExceeded` |
| 16:27 | 修复 runtime-values.yaml 密码覆盖问题 | `helm template` 通过 |
| 16:31 | upgrade（revision 4） | 通过，PVM DaemonSet 开始工作 |
| ~16:40 | 发现 `cube-node-pvm` init `CrashLoopBackOff`（rpm not found） | 修复 rpmPath |
| 16:46 | upgrade（revision 5） | 通过 |
| 16:51 | upgrade（revision 6，`--no-hooks`） | 绕过 `cube-pvm-preflight` 鸡生蛋问题，完成 PVM 内核安装 |
| ~16:55 | 64_host 因 PVM 内核安装重启，内核切换到 `6.6.69-...pvm.host-...` | 节点 Ready |
| 16:58–17:37 | upgrade（revision 7–12） | 逐步修复 helm test 各问题 |
| 17:54 | upgrade（revision 13） | deployed |
| 17:59 | `helm test`（helm-test9.log） | **8/8 Succeeded** |

## 六、关键问题与修复

### 问题 1：`kubectl` 认证失败

- 现象：`kubectl get nodes` 报 `the server has asked for the client to provide credentials`。
- 原因：当前用户本地 kubeconfig 未配置正确凭据。
- 修复：后续所有命令显式使用 `export KUBECONFIG=/etc/kubernetes/admin.conf`。

### 问题 2：无默认 StorageClass，PVC 无法绑定

- 现象：`kubectl get storageclass` 仅有 `doris-be` / `doris-fe`（no-provisioner），无默认类。
- 影响：MySQL、Redis、CubeMaster 的 PVC 会一直 Pending。
- 修复：runtime-values.yaml 中为 `mysql.persistence`、`redis.persistence`、`controlPlane.master.persistence` 显式配置 `hostPath`。

### 问题 3：`cubevs-cidr-preflight` Job 超时（DeadlineExceeded）

- 现象：首次 install 预检 Job 无法调度。
- 原因：master 节点带默认污点 `node-role.kubernetes.io/control-plane:NoSchedule`，Job Pod 无容忍。
- 修复：runtime-values.yaml 增加 `placement.controlPlane.tolerations`（容忍 `cube.tencent.com/control` 与 `node-role.kubernetes.io/control-plane`）。

### 问题 4：`cube-node-pvm` init 容器 CrashLoopBackOff（rpm not found）

- 现象：`/bin/sh: 1: rpm: not found`。
- 原因：`pvm-host-bootstrap.sh` 在 `rpmPath` 有值时优先使用 rpm，即便目标是 Debian 系系统。
- 修复：runtime-values.yaml 将 `bootstrap.pvmHostKernel.package.rpmPath` 指向不存在的路径（`/artifacts/kernel-pvm-host.rpm.unused`），强制脚本走 deb 分支。

### 问题 5：`helm template` 密码校验失败

- 现象：`mysql.password / mysql.rootPassword still equal the CHANGE_ME_* default sentinel`。
- 原因：runtime-values.yaml 中 `mysql` / `redis` 的 `persistence` 被写成并列的重复 key，覆盖了前面生成的密码。
- 修复：将 `persistence` 合并进 `mysql` / `redis` 各自的 key 下。

### 问题 6：`cube-pvm-preflight` 预检 Job 阻塞（鸡生蛋）

- 现象：`cube-pvm-preflight`（pre-upgrade hook）校验 PVM 就绪时，内核尚未被 DaemonSet 安装，hook 失败。
- 修复：首次安装 PVM 内核时使用 `helm upgrade --no-hooks` 跳过 preflight，让 `cube-node-pvm` DaemonSet 完成内核安装后再恢复正常升级。

### 问题 7：`helm test` Pod 全部 Pending

- 现象：health / cubemastercli / mysql / redis / dns / node-image 测试 Pod 均为 Pending，describe 显示 `1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }`。
- 修复：`node-health.yaml` 中所有测试 Pod 增加 `{{- include "cube.controlPlanePlacement" . | nindent 2 }}`，使测试 Pod 可调度到 master。

### 问题 8：`dns-test` 使用 busybox nslookup 失败

- 现象：`nslookup cube.app` 报 `connection timed out; no servers could be reached`，但 `getent hosts` / `curl` 正常。
- 原因：busybox nslookup 与集群 CoreDNS rewrite zone 配合不佳。
- 修复：`dns-test` 改用 `curlimages/curl` 镜像 + `getent hosts`。

### 问题 9：`health-test` / `node-image-test` curl 解析失败

- 现象：`Could not resolve host: cube-master.cube-system.svc.cluster.local`，而 `curl -4` 成功。
- 原因：CoreDNS 对无法回答的 AAAA 查询向上游转发且上游超时；curl 默认先探测 IPv6 导致失败。
- 修复：测试脚本内定义 `curl() { command curl -4 --retry 3 --retry-all-errors --retry-delay 1 "$@"; }` 包装函数，强制 IPv4 并重试。

### 问题 10：`dns-test` getent 偶发失败

- 现象：同一测试偶发失败（CoreDNS 上游查询瞬时超时）。
- 原因：`getent` 单次查询无重试，`set -e` 下直接退出。
- 修复：`a_record()` 包装 5 次重试（每次 sleep 1s）。

### 问题 11：`proxy-control-test` 根路径 400

- 现象：`curl http://cube-proxy.../` 偶发 DNS 失败；HTTPS 返回 `400 {"error":"bad request"}`。
- 原因：CubeProxy 数据面只代理沙箱流量，裸 `/` 路径本就返回 400（设计如此）；且原探针端口/域名用法错误。
- 修复：探针改为访问 admin 健康端点 `GET /admin/healthz`，通过 `secretKeyRef` 注入 `cube-admin-token` 作为 `X-Cube-Admin-Token` 请求头，校验返回 200。
- 补充事实：admin token 在 proxy 的 `global.conf` 中用的是 **secret 解码后的明文**（非 base64 原文）；测试 Pod 经 `secretKeyRef` 挂载时已自动解码，与 proxy 一致。

## 七、验证结果（helm test，v13）

```
TEST SUITE: cube-health-test            Succeeded
TEST SUITE: cube-cubemastercli-test     Succeeded
TEST SUITE: cube-mysql-test             Succeeded
TEST SUITE: cube-redis-test             Succeeded
TEST SUITE: cube-proxy-control-test     Succeeded
TEST SUITE: cube-dns-test               Succeeded
TEST SUITE: cube-node-image-test        Succeeded
TEST SUITE: cube-node-runtime-test      Succeeded
```

### 最终资源状态

```bash
$ kubectl get deploy,ds,sts -n cube-system
deployment.apps/cube-api                1/1   1   1    # :3000 (外部 E2B SDK)
deployment.apps/cube-cubemastercli      1/1   1   1
deployment.apps/cube-lifecycle-manager  1/1   1   1    # :8083
deployment.apps/cube-master             1/1   1   1    # :8089
deployment.apps/cube-ops                1/1   1   1    # :3010 (WebUI /opsapi + SDK)
deployment.apps/cube-proxy              1/1   1   1    # :80/443/8082
deployment.apps/cube-webui              1/1   1   1    # :12088

daemonset.apps/cube-node                1/1   # k8s-node1
daemonset.apps/cube-node-bootstrap      1/1
daemonset.apps/cube-node-installer      1/1
daemonset.apps/cube-node-pvm            1/1   # 内核已装好，steady

statefulset.apps/cube-mysql             1/1
statefulset.apps/cube-redis             1/1
```

## 八、Chart 代码修改（本机工作区）

仅修改 `deploy/kubernetes/chart/templates/tests/node-health.yaml`（测试探针健壮性，可保留/回退）：

1. 所有测试 Pod 增加 `cube.controlPlanePlacement`，解决单主节点 + 污点场景下测试 Pod 无法调度的问题。
2. `health-test` / `node-image-test`：新增 `curl()` 包装函数，强制 `-4` + 重试，规避 CoreDNS AAAA 上游转发超时。
3. `dns-test`：镜像由 busybox 改为 `curlimages/curl`，解析改用 `getent`，并加 5 次重试循环。
4. `proxy-control-test`：探针改为 `GET /admin/healthz`（带 `X-Cube-Admin-Token`），校验 HTTP 200。

## 九、当前状态与访问方式

所有 Service 均为 ClusterIP，集群内无现成 Ingress Controller：

| Service | ClusterIP | 端口 | 说明 |
|---|---|---|---|
| cube-webui | 10.50.122.93 | 12088 | WebUI |
| cube-api | 10.50.194.242 | 3000 | E2B SDK |
| cube-ops | 10.50.250.255 | 3010 | CubeOps |
| cube-master | 10.50.236.252 | 8089 | CubeMaster |
| cube-proxy | 10.50.184.147 | 80/443/8082 | 沙箱流量入口 |
| cube-lifecycle-manager | 10.50.157.246 | 8083 | 生命周期管理 |
| cube-mysql / cube-redis | Headless | 3306/6379 | 存储 |

从外部访问（临时）：

```bash
KUBECONFIG=/etc/kubernetes/admin.conf kubectl -n cube-system port-forward svc/cube-webui 12088:12088
```

沙箱域名 `*.cube.app` 已由 CoreDNS rewrite 指向 `cube-proxy` Service，集群内可访问。

## 十、注意事项

1. **PVM 内核不可逆**：`bootstrap.pvmHostKernel.enabled=true` 会安装宿主内核并修改 GRUB，`helm rollback` 不会撤销内核/GRUB 改动；如需回退需人工处理。
2. **主机重启影响**：PVM 内核安装触发 64_host 重启，期间其上负载（EMQX、Node-RED、gitlab-runner）中断，已获确认接受。
3. **k8s-librax4 NotReady**：该节点未打 cube-node 标签，不影响当前部署；如需扩容需先恢复其 Ready。
4. **凭据安全**：MySQL/Redis 密码与 admin token 均在本记录及 `runtime-values.yaml` 中，涉及敏感信息，注意保管。
5. **升级说明**：控制面组件可滚动升级；计算面（`cube-node` Big Pod）升级会重建并中断沙箱，参考 `docs/zh/guide/kubernetes/upgrade.md`。

## 附录：常用命令速查

```bash
# kubectl 认证
export KUBECONFIG=/etc/kubernetes/admin.conf

# 节点标签（扩容计算节点时）
kubectl label node <node> cube.tencent.com/cube-node=true
kubectl label node <node> cube.tencent.com/allow-pvm-bootstrap=true

# 升级
cd /tmp/cube-deploy
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f deploy/kubernetes/chart/values-cn.yaml -f runtime-values.yaml \
  --wait --timeout 30m

# 绕过 preflight（仅 PVM 首次安装等场景）
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f deploy/kubernetes/chart/values-cn.yaml -f runtime-values.yaml --no-hooks

# 测试
helm test cube -n cube-system --timeout 20m

# 查看组件日志
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c cubelet --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node-pvm -c pvm-host-bootstrap --tail=100
```

---

## 十一、部署后沙箱访问验证与排障（2026-08-03 晚至 08-04）

`helm test` 8/8 通过只证明控制面组件健康。沙箱的真实可用性（SDK 建沙箱、命令执行、网络、生命周期）需要额外验证。本节记录完整排障链。

### 11.1 验证环境

从开发机（`207_songtao`）通过 `ssh 63_host` 打通两条 `kubectl port-forward` 隧道：

```bash
export KUBECONFIG=/etc/kubernetes/admin.conf
nohup kubectl port-forward svc/cube-api   -n cube-system 13000:3000 --address 127.0.0.1 &
nohup kubectl port-forward svc/cube-proxy -n cube-system 18080:80   --address 127.0.0.1 &
```

> **注意**：`kubectl port-forward` 需与 `KUBECONFIG=/etc/kubernetes/admin.conf` 同时使用，否则报 `Unauthorized`；且不会在隧道对端 Pod 重建/滚动更新后自动恢复，需重新拉起。

SDK 环境变量（`cubesandbox` Python SDK）：

```bash
export CUBE_API_URL="http://127.0.0.1:13000"          # 经 port-forward 到 cube-api
export CUBE_TEMPLATE_ID="tpl-b904804bf74a478b84e3636d" # 最终使用的模板
export CUBE_PROXY_NODE_IP="127.0.0.1"                  # 经 port-forward 到 cube-proxy
export CUBE_PROXY_PORT_HTTP="18080"                    # 注意：SDK 默认 80，必须显式覆盖
```

### 11.2 排障链一：模板 envd 端口（命令执行 `ConnectException`）

- 现象：建沙箱成功，但 `commands.run` 报 `e2b_connect.client.ConnectException: (<Code.unknown: 'unknown'>, '')`。
- 根因：SDK 通过 `cube-proxy → 沙箱内 envd:49983` 执行命令。最初用 `cubemastercli template create-from-image --expose-port 8000` 建模板，暴露的容器端口不是 49983。
- 修复：用 `--expose-port 49983` 重建模板。

```bash
cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" \
  template create-from-image --image <image> --expose-port 49983 --writable-layer-size 20Gi
```

> `--writable-layer-size` 为必填，缺省会直接报错；`cubemastercli template render` 语法是 `--template-id <id>` 而非位置参数。

### 11.3 排障链二：沙箱 DNS（公网域名无法解析）

- 现象：沙箱内 `curl https://www.baidu.com` 返回 `000`，`getent hosts` 失败；沙箱内 `resolv.conf` 只有 `nameserver 10.50.0.10`（集群 CoreDNS），且该 IP 的数据面流量被丢弃。
- 根因：CubeNet 的 `alwaysDeniedSandboxCIDRs` 默认拒绝 `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16` 等私网段（安全策略），而 CoreDNS 的 ClusterIP `10.50.0.10` 恰好落在被拒段内——**沙箱 → CoreDNS 的 DNS 查询在数据面被静默丢弃**，与 `runtime-values.yaml` 里 `cubeNode.dns.sandbox` 是否生效无关。
- 修复（两层）：
  1. `runtime-values.yaml` 为沙箱显式配置公网 DNS（并重启 cube-node 让 cubelet 的 `dynamicconf/conf.yaml` 生效）：

     ```yaml
     cubeNode:
       dns:
         sandbox:
           followNodeDns: false
           nameservers:
             - 8.8.8.8
             - 223.5.5.5
     ```

  2. 模板创建时用 `--dns` 把公网 DNS 直接烘焙进 rootfs 的 `resolv.conf`（因为 Cubelet 的 `disable_host_netfile: true` 不会在运行时覆盖 guest 内静态 `resolv.conf`，仅改 cubelet 配置对已建 rootfs 无效）。

     ```bash
     cubemastercli template create-from-image --image <image> \
       --expose-port 49983 --writable-layer-size 20Gi \
       --dns 8.8.8.8 --dns 223.5.5.5
     ```

### 11.4 排障链三：MTU 黑洞（HTTPS 握手超时、大文件截断）

这是最深的排障链，涉及 CubeShim 重编译 + 宿主机 VMM 代码修复。

- 现象（三段式，层层递进）：
  1. DNS 修好后：`curl -s http://www.baidu.com` 返回 `200`，但 `curl -k https://www.baidu.com` 返回 `000`（TLS 握手超时）。
  2. `socket.create_connection(("www.baidu.com", 443))` 能连上（TCP 层通），说明是数据面在 TLS 交互中丢包。
  3. 下载 `1MB.zip` 只收到约 228KB 即断——典型的**载荷超过某阈值被静默丢弃**。
- 根因：**MTU 黑洞**。链条如下：
  - Calico（VXLAN）下 `cube-node` Pod 的 `eth0` MTU = **1450**。
  - 但沙箱 tap（`cube-dev` / `z172.16.x.y`）与 guest 内 `eth0` MTU = **1500**（打包默认 `mvm_mtu=1500`）。
  - guest 通告 MSS=1460（1500−40），回程 TCP 段一旦超过 Pod MTU（1450−40=1410 有效载荷）就会被 Calico 数据面**静默丢弃**。
  - 表现：TLS ServerHello 之后的大段（证书）到不了客户端 → 握手超时；大文件下载截断。
- 修复（代码 + 镜像 + 节点三层）：

  **a. Helm Chart 增加 MTU 参数（已合入工作区）**
  - `deploy/kubernetes/chart/values.yaml`：新增 `cubeNode.network.mtu`（默认 `0` 表示用打包默认值）。
  - `deploy/kubernetes/chart/templates/_helpers.tpl`：`cube.nodeComponentCommonEnv` 注入 `CUBE_SANDBOX_NETWORK_MTU`。
  - `deploy/kubernetes/images/scripts/cube-node-entrypoint.sh` 与 `component-entrypoint.sh`：启动时用 `sed` 把 `config.toml` 里的 `mvm_mtu` 打补丁为配置值（含整型校验，防注入）。

  ```yaml
  # runtime-values.yaml 追加
  cubeNode:
    network:
      mtu: 1450          # 必须与 Pod 网络 MTU（此处 Calico 1450）一致
  ```

  **b. CubeShim 透传 MTU 到虚拟机（已合入工作区，需重编译 cube-shim）**
  - `CubeShim/shim/src/hypervisor/config.rs` `add_nets`：显式 `nc.mtu = Some(n.mtu as u16)`，使 VIRTIO_NET_F_MTU 通知 guest 应用通告 MTU。
  - `CubeShim/shim/src/common/utils.rs` `restore_nets_config`：快照恢复路径同样透传 MTU。
  - `hypervisor/vmm/src/config.rs` `update_nets`：补 `mtu_map`，把 MTU 从输入 `NetConfig` 正确复制到 VM 的 `NetConfig`（此前只更新 tap / rate_limiter，快照恢复会丢掉 MTU）。

  **c. 64_host 编译并替换 cube-shim 二进制**

  ```bash
  # 64_host 上：缺的编译依赖先装上
  apt-get install -y build-essential libseccomp-dev libcap-ng-dev

  # 编译（CubeShim 工作区已同步到 64_host）
  cd /opt/cube-build/CubeShim
  cargo build --release

  # 替换节点二进制（containerd-shim-cube-rs + cube-runtime）
  /tmp/deploy-shim.sh   # 把 target/release/{containerd-shim-cube-rs,cube-runtime}
                        # 复制到 /usr/local/services/cubetoolbox/cube-shim/bin/
  # 重启 cubelet 容器使新 shim 生效
  kubectl -n cube-system rollout restart ds/cube-node
  ```

  > 首次编译曾报 `error: linking with 'cc' failed: cannot find -lseccomp / -lcap-ng`，安装 `libseccomp-dev libcap-ng-dev` 后解决。

  **d. 重建模板（关键！）**
  - 模板创建流程会先起"build 沙箱"再打快照。若 build 沙箱是**从旧模板快照恢复**的，快照里的 guest `eth0` MTU 仍是 1500，新 shim 的 `update_nets` 修复恰好覆盖了这一路径。
  - 但更稳妥的做法是重新建一个模板（不带 `--node` 让 CubeMaster 自动选节点），确保 build 沙箱冷启动，使新 shim 的 MTU 透传生效。重建后验证：

    ```bash
    # 沙箱内
    ip link show eth0        # mtu 1450
    curl -k https://www.baidu.com  # 200，TLS 正常
    ```

- 验证成果：guest `eth0` MTU 从 1500 → **1450** 后，HTTPS 握手与大文件下载全部恢复正常；最终模板 `tpl-b904804bf74a478b84e3636d`。

### 11.5 排障链四：auto_resume 504（CubeProxy 心跳注册写向过期 Redis Pod IP）

- 现象：`verify_cubesandbox.py` 步骤 10 中，auto_pause（35s 后自动暂停）正常、`get_info` 也显示 `paused`，但随后触发 auto_resume 的命令全部 `504 Gateway Time-out`（来自 openresty），恢复后状态仍 `paused`。
- 定位（逐层排除）：
  - 手动复现：单独建 auto_pause 沙箱 → paused → resume，一切正常。说明功能本身没坏，是**脚本运行环境特定状态**导致。
  - CubeProxy error.log 无该沙箱 gate 记录 → gate 从未走 resume 分支（状态字典里没有该沙箱的 `paused`）。
  - CLM 日志无对应 `resume request received` → **CLM 根本没收到 resume 请求**。
  - `$cube_sidecar_addr = cube-lifecycle-manager.cube-system.svc.cluster.local:8083` 可从 cube-proxy 内连通（healthz 200）→ 排除链路不通。
  - 结论：**CubeProxy 本地状态字典 `cube_sandbox_state` 从未被填充**，gate 直接放行请求到已暂停的 backend → 30s 无响应 → 504。
- 根因：**CLM 的服务发现 fleet 为空**。
  - CubeProxy 通过 `proxy_registry.lua`（`ngx.timer` 里用 cosocket 连 Redis）写 `cube:v1:shared:cube_proxy:*` 心跳，供 CLM 发现。
  - 该 Redis 地址由 Helm chart 渲染时 `lookup` 固化：本环境 Redis 是 **headless Service + StatefulSet**，chart 走了 `Endpoints` 分支，把当时的 Pod IP `10.60.235.237` 硬编码进 `CUBE_PROXY_REGISTRY_REDIS_HOST`。
  - Redis Pod 重建后 IP 变为 `10.60.235.240`，但 cube-proxy 的 env 是静态的 → 心跳一直写向**已不可达的旧地址** → Redis 里 `cube:v1:shared:cube_proxy:*` 永远不存在 → CLM `broadcast` 对空 fleet 静默跳过（仅 Debug 日志，无 warn）→ proxy 状态字典一直为空。
- 修复（chart + 现网）：
  1. **Chart 修复（已合入工作区）** `deploy/kubernetes/chart/templates/proxy.yaml`：headless Service 分支不再 `lookup` Endpoints IP，改为 StatefulSet Pod DNS 名：

     ```yaml
     {{- $redisRegistryHost = printf "%s-0.%s.%s.svc.%s" (include "cube.redisName" .) (include "cube.redisName" .) .Release.Namespace (include "cube.clusterDomain" .) -}}
     ```

     即 `cube-redis-0.cube-redis.cube-system.svc.cluster.local`，由 `cube-proxy-entrypoint.sh` 启动时 `getent` 解析成当前存活 IP（cosocket 无 nginx resolver，必须在启动时解析好）。

  2. **现网立即修复**：

     ```bash
     kubectl set env deploy/cube-proxy -n cube-system \
       CUBE_PROXY_REGISTRY_REDIS_HOST=cube-redis-0.cube-redis.cube-system.svc.cluster.local
     kubectl rollout status deploy/cube-proxy -n cube-system --timeout=90s
     ```

- 验证：
  - Redis 中出现 `cube:v1:shared:cube_proxy:registry` + `heartbeat`，心跳持续刷新。
  - CLM 日志：`discovery: proxy joined` → `replay begin entries=3` → `replay done pushed:3, failed:0`。
  - 复跑 `verify_cubesandbox.py`：步骤 10 auto_resume **通过**。

### 11.6 验证脚本最终结果（34/34 通过）

```bash
cd /workspace/working/mega-agent/scripts/cube
export CUBE_API_URL="http://127.0.0.1:13000"
export CUBE_TEMPLATE_ID="tpl-b904804bf74a478b84e3636d"
export CUBE_PROXY_NODE_IP="127.0.0.1"
export CUBE_PROXY_PORT_HTTP="18080"
python3 -u verify_cubesandbox.py
```

```
结果: 34/34 通过, 0 失败
```

覆盖项：创建/销毁沙箱、基础命令（bash/python/R/node/envd）、numpy/pandas、公网 HTTP(S)+DNS+urllib、文件读写（UTF-8/emoji）、目录操作、错误处理、超时处理、pause/resume 与 writable layer 保留、auto_pause/auto_resume、envd 日志、hostPath 挂载持久化。

### 11.7 本次代码修改汇总（工作区，未提交）

| 文件 | 修改 |
|---|---|
| `CubeShim/shim/src/hypervisor/config.rs` | `add_nets` 透传 MTU（VIRTIO_NET_F_MTU） |
| `CubeShim/shim/src/common/utils.rs` | `restore_nets_config` 快照恢复透传 MTU |
| `hypervisor/vmm/src/config.rs` | `update_nets` 补 MTU 复制（快照恢复丢 MTU） |
| `deploy/kubernetes/chart/values.yaml` | 新增 `cubeNode.network.mtu`（默认 0） |
| `deploy/kubernetes/chart/templates/_helpers.tpl` | 注入 `CUBE_SANDBOX_NETWORK_MTU` 环境变量 |
| `deploy/kubernetes/images/scripts/cube-node-entrypoint.sh` / `component-entrypoint.sh` | 启动时 patch `mvm_mtu`（含整型校验） |
| `deploy/kubernetes/chart/templates/proxy.yaml` | CubeProxy registry 的 Redis 地址改用 StatefulSet Pod DNS 名（防 IP 过期） |
| `deploy/kubernetes/chart/templates/tests/node-health.yaml` | 测试 Pod 增加 control-plane 容忍；curl 强制 IPv4+重试；`dns-test` 改用 `getent`；`proxy-control-test` 探 `/admin/healthz` 带 token |

### 11.8 遗留与注意

1. `kubectl port-forward` 不会随目标 Pod 重建自动恢复，运行验证前先确认两条隧道（13000/18080）存活。
2. `CUBE_PROXY_PORT_HTTP` 必须显式设置：SDK 默认 `80`，指向 63_host 上的其它 nginx 而非 cube-proxy，会得到 404。
3. 沙箱 DNS 公网化后，集群内私网域名（如内网服务）需另行在 `cubeNode.dns.sandbox` 或模板 DNS 中补充，否则会走 `alwaysDeniedSandboxCIDRs` 被拒。
4. 本次对 CubeShim/VMM 的修改需要重新构建镜像（`deploy/kubernetes/images/build-cube-images.sh`）才能在其它节点/正式发布中生效；64_host 上为临时替换节点二进制。后续发布请将 `mvm_mtu` 配置、CubeShim MTU 透传、CubeProxy registry 修复一并合入。
