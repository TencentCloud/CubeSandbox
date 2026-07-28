# EgressProxy 本地 veth 测试报告

[English](./EGRESS_TEST_EN.md) | 中文

## 1. 结论

2026-07-28 在测试集群 `cls-d3bwloeq` 对
[EGRESS_VETH.md](./EGRESS_VETH.md) 方案执行实现验证。

本报告只验证数据路径能够受用户自定义 NetworkPolicy 管理。策略内容完全由
用户决定；测试策略仅为验收临时创建并在测试后删除，不属于 Chart 资源，也
不是产品内置或推荐策略。

## 2. 测试环境

| 项目 | 值 |
| --- | --- |
| 集群 | `cls-d3bwloeq` |
| CNI | Cilium |
| Helm Release | `cube`，最终验收 Revision 65 |
| 计算节点 | `10.0.99.2`、`10.0.99.21`、`10.0.99.119` |
| Proxy 镜像 | `20260728-review04` |
| Configurer 镜像 | `20260728-review06` |
| Cubelet / NetworkAgent 镜像 | `20260728-cgroupv2-01` |
| 配置的集群 CIDR | `9.165.0.0/24` |
| 测试 sandbox | `8032765ce82f4039b16a815e847958a5` |
| sandbox 节点/IP | `10.0.99.119` / `172.16.0.196` |

## 3. 静态和本地测试

以下检查通过：

- Helm lint；
- Chart guard；
- 主备粘滞切换测试；
- policy rule、route table 和 iptables 所有权冲突测试；
- 集群 CIDR 规范化及去重测试；
- 仅删除本组件宿主机状态的清理测试；
- privileged network namespace 中的真实 veth 生命周期测试；
- Big Pod 进程迁移的 cgroup v1 `tasks` 和 v2 `cgroup.procs` 双模式测试；
- 固定宿主接口所有权和冲突失败测试；
- Shell 语法和 `git diff --check`；
- Proxy、Configurer 镜像不包含隧道工具或依赖。

Chart 渲染出的 EgressProxy 常驻资源只包含：

- `cube-egress` namespace；
- Primary、Standby 两个 Proxy DaemonSet；
- Configurer DaemonSet。

渲染结果不包含 EgressProxy StatefulSet、Service、PDB、NetworkPolicy 或隧道
Secret。

## 4. 数据路径

三个计算节点均具备：

```text
cube-egress-p0  169.254.240.1/30
cube-egress-p1  169.254.240.5/30
10900: from 172.20.0.2 fwmark 0x1000/0x1000
       iif cube-router lookup 100
```

活动路由示例：

```text
default via 169.254.240.2 dev cube-egress-p0 metric 100
unreachable default metric 32767
```

真实 sandbox 访问 `allowed-svc` 成功。节点 mark、veth FORWARD 和 Proxy SNAT
计数同步增加；目标 Nginx 观察到的源地址为同节点 Primary Proxy PodIP
`9.165.0.130`。

访问非集群 CIDR 时 `CUBE-EGRESS-MARK` 计数不增加，证明请求不进入
EgressProxy。测试集群的独立网络策略可能继续拒绝该目标，这不改变路径判定。

## 5. 用户 NetworkPolicy

测试人员自行定义临时策略，结果如下：

| 用例 | 结果 |
| --- | --- |
| sandbox → allowed Service | 成功 |
| sandbox → denied Service | 超时，符合用户策略 |
| 普通 Pod → denied Service | 成功，不受 Proxy 策略影响 |
| 目标服务观察来源 | EgressProxy PodIP |

策略测试完成后已删除。Chart 安装、升级和卸载不管理用户 NetworkPolicy。

## 6. 主备和故障关闭

在 sandbox 所在节点执行真实接口故障：

| 场景 | 结果 |
| --- | --- |
| Primary veth down | 活动路由切换到 Standby |
| Standby 接管后访问 Service | 成功 |
| 服务端观察来源 | Standby PodIP `9.165.0.153` |
| Primary 恢复 | 保持当前健康 Standby，不主动回切 |
| Primary、Standby 均 down | 删除活动默认路由，只保留 `unreachable default` |
| 双故障访问集群 CIDR | 超时，不回落主路由 |
| 两端恢复 | 自动恢复活动路由和访问 |

此外，在 veth 保持 UP 的情况下暂停 Primary Proxy 健康服务进程，Configurer
通过 TCP 健康检查识别到数据面进程失效，并在 10 秒内切换到 Standby。恢复
Primary 后仍保持 Standby，符合粘滞切换设计。

## 7. Cube Big Pod 更新

本节来自同日较早的独立 sandbox 轮次，不与第 2 节最终性能 sandbox 混用。
通过 annotation 变更真实滚动三个 `cube-node` Pod。sandbox 所在节点更新前后：

| 检查项 | 更新前 | 更新后 |
| --- | --- | --- |
| sandbox ID | `d6d9...2eb8` | 不变 |
| sandbox 创建时间 | `2026-07-28 09:47:49` | 不变 |
| shim PID | `1877986` | 不变 |
| network-agent PID | `261923` | 不变 |
| Primary veth ifindex | `659` | 不变 |
| Standby veth ifindex | `658` | 不变 |
| table 100 活动路由 | Primary | Primary |

滚动后 sandbox 仍能访问集群 Service。

## 8. 性能

性能测试使用同一 sandbox 和同一份 64 MiB 文件，分别覆盖同节点/跨节点及
PodIP/Service IP。直连基线仅在测试期间暂停该节点 Configurer 调和，并在
`CUBE-EGRESS-MARK` 顶部临时插入集群 CIDR 的 `RETURN`；测试结束后立即删除
临时规则、恢复调和，并确认正式 mark 规则和 table 100 路由已恢复。

吞吐基准交错测试 PodIP 和 Service IP，使用相同请求脚本及样本数，避免顺序
执行带来的 CPU 调度和缓存偏差。

### 8.1 新建连接延迟

每个目标、每条路径各执行 200 次新建 TCP 连接和 HTTP 请求：

| 目标 | 直连 P50 | Proxy P50 | 额外 P50 | 直连 P99 | Proxy P99 | 额外 P99 | 错误率 | 结果 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 同节点 PodIP | 20.818 ms | 21.720 ms | 0.902 ms | 70.876 ms | 71.656 ms | 0.780 ms | 0/200 | 通过 |
| 同节点 Service IP | 21.956 ms | 21.305 ms | -0.651 ms | 73.322 ms | 71.599 ms | -1.723 ms | 0/200 | 通过 |
| 跨节点 PodIP | 21.892 ms | 21.790 ms | -0.102 ms | 72.313 ms | 71.677 ms | -0.636 ms | 0/200 | 通过 |
| 跨节点 Service IP | 21.653 ms | 21.958 ms | 0.305 ms | 71.759 ms | 72.171 ms | 0.412 ms | 0/200 | 通过 |

门槛为额外 P50 不超过 1 ms、额外 P99 不超过 5 ms、错误率低于 0.01%。

### 8.2 已建立路径延迟

对同一跨节点 PodIP 分别发送 200 个 ICMP 请求：

| 指标 | 直连 | EgressProxy | 额外开销 |
| --- | ---: | ---: | ---: |
| RTT P99 | 0.799 ms | 0.845 ms | 0.046 ms |
| 折算单向额外延迟 | - | - | 约 0.023 ms |

结果满足单向额外 P99 不超过 2 ms 的门槛。

### 8.3 吞吐

单连接每轮下载 64 MiB；4 并发场景每个连接下载 64 MiB，即每轮聚合传输
256 MiB。表中使用耗时中位数：

| 目标 | 并发 | 直连耗时 | Proxy 耗时 | 直连吞吐 | Proxy 吞吐 | 吞吐比例 | 门槛 | 结果 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 同节点 PodIP | 1 | 46.875 ms | 51.440 ms | 11.453 Gbit/s | 10.437 Gbit/s | 91.13% | ≥90% | 通过 |
| 同节点 PodIP | 4 | 193.324 ms | 220.470 ms | 11.108 Gbit/s | 9.740 Gbit/s | 87.69% | ≥85% | 通过 |
| 同节点 Service IP | 1 | 52.145 ms | 49.930 ms | 10.296 Gbit/s | 10.752 Gbit/s | 104.44% | ≥90% | 通过 |
| 同节点 Service IP | 4 | 198.843 ms | 228.591 ms | 10.800 Gbit/s | 9.394 Gbit/s | 86.99% | ≥85% | 通过 |
| 跨节点 PodIP | 1 | 216.868 ms | 169.128 ms | 2.476 Gbit/s | 3.174 Gbit/s | 128.23% | ≥90% | 通过 |
| 跨节点 PodIP | 4 | 1.441 s | 1.459 s | 1.490 Gbit/s | 1.472 Gbit/s | 98.80% | ≥85% | 通过 |
| 跨节点 Service IP | 1 | 198.609 ms | 195.350 ms | 2.703 Gbit/s | 2.748 Gbit/s | 101.67% | ≥90% | 通过 |
| 跨节点 Service IP | 4 | 1.471 s | 1.506 s | 1.460 Gbit/s | 1.426 Gbit/s | 97.68% | ≥85% | 通过 |

有效吞吐按 `有效负载字节数 × 8 ÷ 耗时` 计算，不包含 TCP/IP、以太网封装和
HTTP 响应头开销。吞吐比例按
`EgressProxy 有效吞吐 ÷ 直连有效吞吐` 计算，等价于相同流量下的
`直连耗时 ÷ EgressProxy 耗时`。

结果表明本地 veth 链路消除了旧数据路径的主要吞吐损失。

## 9. 长连接

健康路径使用同一 TCP 连接持续发送 HTTP/1.1 keep-alive 请求：

```text
duration=618.017s
ok=605
fail=0
```

探针持续超过 10 分钟且无失败。另一次与性能直连基线并行的探针因测试规则主动
改变同一 Service 的路由而中断，已作为测试污染排除；独立复测期间未修改路由。

## 10. 集群验收

- 执行 `egressProxy.enabled=true → false` 后，三个计算节点的 priority 10900
  规则、table 100 路由和 `CUBE-EGRESS*` 规则均被 Pod `preStop` 清理；
- 再次启用后，三个节点均重新建立预期数据路径；
- 三个 Primary、三个 Standby Proxy Pod 全部 Ready；
- 三个 Configurer Pod 全部 Ready；
- EgressProxy 回归完成后执行 Helm test，8 个 suite 均为 `Succeeded`；
  `cube-proxy-control-test` 使用 Secret 中的管理令牌访问
  `/admin/healthz`，严格校验 HTTP 2xx；
- 集群中不存在旧 EgressProxy StatefulSet、Service、PDB、NetworkPolicy、
  隧道 Secret 或旧接口；
- Helm release values 的 `egressProxy` 只有 `enabled` 和 `clusterCIDRs`。

## 11. 测试资源

通用服务和客户端见
[egress-test-resources.yaml](../scripts/egress-test-resources.yaml)，长连接脚本见
[egress-long-connection.sh](../scripts/egress-long-connection.sh)。

用户 NetworkPolicy 不保存在上述资源文件中，避免被误认为产品内置策略。
验收结束后已销毁测试 sandbox，并删除测试工作负载和 `cube-egress-test`
namespace；产品 `cube-egress` namespace 中无测试 NetworkPolicy。
