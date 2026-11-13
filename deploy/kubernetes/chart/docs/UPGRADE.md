# EgressProxy 升级与回滚指南

[English](./UPGRADE_EN.md) | 中文

本文只覆盖可选的节点本地 veth EgressProxy（`egressProxy.enabled`）。本文不描述
Big Pod 替换：该行为由 base Chart 提供，并由官方
[升级文档](https://cubesandbox.com/zh/guide/kubernetes/upgrade) 说明，本 PR 没有
修改它。

`egressProxy` 与 `cubeNode.hostNetwork`、cubelet PID、sandbox cgroup、hostPath
布局、shim 重连都无关。开启或关闭它只会增加或移除 EgressProxy 数据面。

## 1. 该特性安装了什么

`egressProxy.enabled=true` 时，Chart 新增：

| 资源 | 网络 | 作用 |
| --- | --- | --- |
| `<release>-egress-configurer` DaemonSet | host 网络 | 调和宿主 iptables、ip rule 和 table 10901；选择健康 Proxy |
| `<release>-egress-proxy-primary` DaemonSet | Pod 网络 | 持有 `cube-egress-p0`，转发并 SNAT 为自身 PodIP |
| `<release>-egress-proxy-standby` DaemonSet | Pod 网络 | 持有 `cube-egress-p1`，转发路径同上 |
| `cube-egress` namespace | — | 承载上述三个 DaemonSet，卸载时保留 |

三个 DaemonSet 都使用 `OnDelete`。对外有 `egressProxy.enabled`、
`egressProxy.clusterCIDRs` 和 `egressProxy.routeTable`（默认 10901）三个配置项；
priority 10900、mark、veth 名称和 link-local 前缀固定，不可配置。

`routeTable` 之所以要独立可配：表 100 归 CubeEgress 所有（L7 TPROXY 的
`local default dev lo`），101–110 常被 Cilium/kube-router 占用，200–252 常被
Calico/systemd-networkd 占用，253–255 是内核保留。默认 10901 位于应用私有高位段，
与固定 rule priority 10900 相邻便于识别；若节点上已被占用，改这个值即可。

`egressProxy.enabled=true` 要求 `cubeNode.network.cubeRouter.enabled=true`，否则
Chart 校验直接失败。Cube Router 会向 Big Pod template 注入环境变量，因此**首次
开启会替换一次 Big Pod**。EgressProxy 资源本身与这次替换无关，但发布编排需要
把它算进去。

`clusterCIDRs` 接受 1~32 条，只支持 IPv4。

## 2. 升级边界与限制

只有目标命中 `clusterCIDRs` 的 sandbox 流量才会被标记并进入 table 10901，其余流量
继续走节点主路由。

以下限制对用户可见，必须如实告知：

- 策略粒度是 EgressProxy Pod / 节点，无法按单个 sandbox 区分；
- 主、备不共享 conntrack，切换会中断已建立连接；
- 只支持 IPv4，不支持双栈；
- `clusterCIDRs` 需手工维护，必须覆盖全部 Pod 与 Service CIDR；
- Chart 不创建、不修改、不删除、也不推荐任何 NetworkPolicy；
- Configurer 以 host 网络运行并需要 `NET_ADMIN`，Proxy 的 initContainer 需要
  privileged，因此必须有范围受限的 Pod Security admission 豁免；
- 没有自动 drain、发布编排或指标端点；
- 与 CubeEgress 共存，不牺牲 L7 拦截，具体见第 3 节。

## 3. 与 CubeEgress 共存

CubeEgress 的 L7 透明拦截与 EgressProxy 可以同节点同时运行，`cube-egress-net`
sidecar 始终随 Big Pod 部署，不会因为开启 EgressProxy 而被移除。

两者靠三处隔离保证互不干扰：

| 维度 | CubeEgress | EgressProxy |
| --- | --- | --- |
| 入口 | `-i cube-dev` | `-i cube-router -s <router-nat-ip>` |
| iptables 链 | `TRANSPROXY`（mangle） | `CUBE-EGRESS-MARK` / `CUBE-EGRESS` / `CUBE-EGRESS-SNAT` |
| fwmark | `0xCE010000` / `0xCE020000`，mask `0xFFFF0000` | `0x1000`，mask `0x1000` |
| 路由表 | 100（`local default dev lo`） | 10901（默认） |

分流语义由 CubeVS 的 `classify_egress_flow()` 决定，优先级是：

1. 命中 L7 规则的流 → 打 L7 mark 并注入 `cube-dev`，走 CubeEgress，**不进入**
   EgressProxy；
2. 其余发往 `clusterCIDRs` 的流 → 打 `0x1000` 并进入 `cube-router`，走 EgressProxy；
3. 其余流量 → 节点主路由。

因此 L7 策略优先于 NetworkPolicy；一个被 L7 规则接管的 Service 即使被
NetworkPolicy 拒绝，仍会按 L7 策略放行。需要 NetworkPolicy 拒绝的 Service，
不要给它配置 L7 规则。

确认共存是否生效：

```bash
# L7 链计数器应随 L7 流量增长
iptables -t mangle -L TRANSPROXY -v -n
# 两条链路的路由表应同时存在
ip rule show | grep -E 'lookup (100|10901)'
```

## 4. 发布前检查

```bash
helm lint deploy/kubernetes/chart
bash deploy/kubernetes/chart/scripts/test-egress-proxy-guard.sh
bash deploy/kubernetes/chart/scripts/test-egress-policy-rules.sh
bash deploy/kubernetes/chart/scripts/test-egress-cleanup.sh
```

用生产 values 渲染一次，确认：

- EgressProxy 镜像 tag 或 digest 符合本次发布；
- 只渲染 Configurer、primary、standby 和 namespace；
- 渲染结果不含 EgressProxy 的 NetworkPolicy、隧道 Secret、Service、StatefulSet
  或 PodDisruptionBudget；
- `clusterCIDRs` 覆盖目标集群全部 Pod 与 Service CIDR，且不与 sandbox CIDR 重叠。

每个目标节点在发布前确认：

- 两个 Proxy Pod 与 Configurer 均 Ready；
- 当前活动 Peer 健康检查通过；
- mangle 链在 Service DNAT 之前只标记 `clusterCIDRs`；
- 策略规则同时匹配 cube-router 入接口、Router NATIP 和 route mark；
- table 10901 同时保留活动默认路由和 fail-closed 的 `unreachable default`。

## 5. 推荐发布顺序

三个 EgressProxy DaemonSet 都是 `OnDelete`，`helm upgrade` 不会自动替换 Pod。
逐节点处理：

1. 替换并验证 Configurer，确认宿主规则和活动路由保持正常；
2. 替换非活动 Proxy；
3. 确认新 Proxy Ready 且具备接管能力；
4. 将活动路径切换到已更新的 Proxy；
5. 替换原活动 Proxy；
6. 全程保证节点上至少一个 Proxy 健康，故障切换后不主动回切。

```bash
kubectl -n cube-egress get daemonset
kubectl -n cube-egress delete pod <old-egress-configurer-pod>
kubectl -n cube-egress delete pod <old-inactive-egress-proxy-pod>
```

不要在同一个节点同时删除两个 Proxy：双路径都 down 时 table 10901 只剩
`unreachable default`，被标记的集群流量按设计 fail closed。

## 6. 验收

每个更新过的节点检查：

- Configurer、primary、standby 均 Ready；
- DNAT 前的 mark 仍只命中 `clusterCIDRs`；
- table 10901 的活动默认路由指向预期 veth；
- 用户 NetworkPolicy 的允许用例成功、拒绝用例被拦截；
- 普通 Pod、host 网络 Pod 与节点自身流量未被 EgressProxy 规则误伤；
- 停掉两个 Proxy 后，被标记的集群流量超时，而不是回落到主路由；
- Big Pod 替换后，既有 veth ifindex 与 table 10901 活动路径保持不变。

readiness 不能证明策略已生效，必须执行业务侧允许与拒绝双向抽检。

## 7. 关闭与卸载

把 `egressProxy.enabled` 改为 `false` 会移除三个 DaemonSet。Configurer 的
`preStop` 会执行带所有权校验的清理：删除自有 chain、固定 FORWARD 规则、
priority 10900 策略规则和 table 10901 路由。

```bash
helm upgrade <release> deploy/kubernetes/chart -n <namespace> \
  -f runtime-values.yaml --wait
```

Configurer Pod 消失后核对：

```bash
ip rule show | grep 10900
ip route show table 10901
iptables-save | grep CUBE-EGRESS
```

Chart 不再提供 `post-delete` 清理 Hook。卸载依赖 Configurer Pod 的 `preStop`：
Helm 删除 DaemonSet 后 kubectl 会逐个终止 Pod，每个 Pod 在
`terminationGracePeriodSeconds: 60` 内完成清理。因此卸载**必须**使用
`--wait`，并在其返回后逐节点执行上面的三条核对命令。

强制删除 Pod、节点不可达或绕过 kubelet 时不会执行 `preStop`。这类场景需要在
节点恢复后手工运行同一清理镜像；不宣称自动清理。

## 8. 回滚

```bash
helm history <release> -n <namespace>
helm rollback <release> <revision> -n <namespace> --wait --timeout 10m
```

Helm 只回滚 Kubernetes 资源，不会恢复宿主的 veth、iptables、策略规则或
table 10901 路由。这些只有在新 values 把 `egressProxy.enabled` 改为 `false`
或卸载 release 时才会清理。

`rp_filter` 不在回滚范围内，也不需要回滚：它只写在 `cube-egress-p0/p1` 两个
接口上，接口随 Proxy Pod 删除而消失，节点全局基线与 CubeEgress 都不受影响。

回滚 Proxy 镜像同样按第 5 节的手工顺序执行：回滚后继续使用当前健康路径，
不主动回切。

## 9. 上线门禁

满足以下条件后才能推广：

1. Helm lint、template 与 guard 脚本通过。
2. 至少一个节点完成开启、Proxy 镜像更新、关闭、再开启的完整循环。
3. 用户 NetworkPolicy 的允许与拒绝结果均符合策略定义。
4. 清理后节点上不残留 `CUBE-EGRESS*` 链、priority 10900 规则或 table 10901 路由。
5. Pod Security admission 豁免已获批并记录。
6. 已记录镜像、values、Helm revision 和实测 CNI 版本。
