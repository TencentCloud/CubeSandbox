---
title: "Cube Sandbox 安全沙箱网络技术解析"
date: 2026-06-23
author: Cube Sandbox 团队
description: "AI Agent 赋予机器自主执行能力，也打开了数据泄露与凭证滥用的潘多拉魔盒。CubeSandbox 以 KVM MicroVM 隔离为基座、eBPF 内核态网络为骨架、L7 代理深度检查为护盾，构建了一套从虚拟交换到应用层审计的端到端网络安全体系。本文逐层拆解 CubeVS、CubeProxy、CubeEgress 等核心组件的设计与实现，揭示其如何在开放执行与安全可控之间取得平衡。"
featured: true
weight: 3
---

# Cube Sandbox 安全沙箱网络技术解析

AI Agent 赋予机器自主执行能力，也打开了数据泄露与凭证滥用的潘多拉魔盒。CubeSandbox 以 KVM MicroVM 隔离为基座、eBPF 内核态网络为骨架、L7 代理深度检查为护盾，构建了一套从虚拟交换到应用层审计的端到端网络安全体系。本文逐层拆解 CubeVS、CubeProxy、CubeEgress 等核心组件的设计与实现，揭示其如何在开放执行与安全可控之间取得平衡。

## 1. 业务背景与需求场景

### 1.1 AI Agent 的安全挑战

AI Agent 正在从"对话式助手"演进为"自主执行者"——它能够编写代码、调用 API、操作浏览器、执行系统命令。这种自主性在带来效率飞跃的同时，也引入了前所未有的安全挑战：

| 风险维度 | 典型场景 | 潜在影响 |
|---|---|---|
| **代码执行风险** | LLM 生成的代码包含恶意操作（`rm -rf /`、反向 Shell） | 宿主机被攻陷，影响同租户 |
| **数据泄露** | Agent 将用户隐私/企业机密发送到未授权的外部 API | 合规违规、商业损失 |
| **凭证滥用** | Agent 窃取或滥用 API Key/Token 进行越权操作 | 资源盗用、服务瘫痪 |
| **供应链攻击** | Agent `pip install` 了恶意包，执行了挖矿/后门代码 | 横向渗透、持久化入侵 |
| **逃逸攻击** | 利用内核漏洞从容器逃逸到宿主机 | 整个集群沦陷 |

传统容器方案（Docker/Containerd）**共享宿主机内核**，一旦存在内核漏洞，攻击者可从容器内直接逃逸到宿主机。对于 AI Agent 这种执行**不可信代码**的场景，共享内核的隔离模型已不再安全。

### 1.2 网络安全需求

AI Agent 沙箱的网络层需要解决以下核心问题：

1. **隔离性**：沙箱之间、沙箱与宿主机之间的网络必须完全隔离
2. **可控性**：精确控制每个沙箱可以访问哪些外部服务（IP/域名/协议）
3. **可观测性**：所有出站流量必须可审计、可追溯
4. **透明性**：对 Agent 代码零侵入，不需要修改应用逻辑
5. **高性能**：网络策略不能成为千级并发的瓶颈

## 2. CubeSandbox 架构总览

### 2.1 CubeSandbox 系统架构

CubeSandbox 采用**清晰的分层架构**，从上到下依次为 API 网关层、编排调度层、计算节点层和虚拟化层：

![CubeSandbox 系统架构](./assets/2026-06-23-cubesandbox-network-deep-dive/01-system-architecture.jpg)

### 2.2 CubeSandbox 网络架构

网络数据面包括访问沙箱流量和沙箱外访流量，四层转发及策略执行由高性能的内核态 ebpf 实现，七层路由及深度检查由用户态 nginx/openresty 完成：

- **沙箱 Ingress 流量**
  1. CubeProxy：基于 nginx/openresty 的 CubeProxy 反向代理，通过 http header 信息高性能路由转发。
  2. CubeVS（Ingress）：纯内核态 ebpf 高性能转发，实现 portmapping 的无状态转换逻辑。

- **沙箱 Egress 流量**
  1. CubeVS（Egress）：纯内核态 ebpf 高性能转发，实现 IP 过滤、域名过滤、SNAT、会话追踪等沙箱外访能力。
  2. CubeEgress：基于 nginx/openresty 的 CubeEgress 的 7 层代理，实现 HTTPS 拦截、凭证注入、访问审计等安全能力，还可以基于 lua 扩展。

![CubeSandbox 网络架构](./assets/2026-06-23-cubesandbox-network-deep-dive/02-network-architecture.jpg)

### 2.3 CubeSandbox 宿主机网络管理

network-agent 实现 TAP 池化管理与分配、PortMapping 端口分配、转发策略数据下发、状态持久化等管理。

- **Tap 设备池化**：预创建 500+ 的 Tap 设备，沙箱启动时无需初始化 Tap 设备，减少沙箱创建耗时 59+ms。
- **CubeVS、CubeEgress 转发策略配置数据下发管理**，按用户定义的策略处理网络流量。

![CubeSandbox 宿主机网络管理](./assets/2026-06-23-cubesandbox-network-deep-dive/03-host-network-management.jpg)

## 3. CubeSandbox 沙箱网络虚拟化：CubeVS

### 3.1 设计目标

传统容器网络方案（Linux Bridge、OVS、iptables NAT）的每包处理开销随租户数增长而膨胀。CubeVS 用三个小型 eBPF 程序替代了整条传统网络栈，实现了 ARP 代理、策略检查、L7 代理选择、SNAT、会话创建、反向 NAT、端口映射代理、DNAT 到沙箱 IP、保持透明代理源 IP。

- **点对点低延迟**：每个沙箱独占 TAP 设备，无共享网桥、无软件交换机跳转，没有共享 L2 域，无需 ARP 洪泛和 STP。
- **内核态策略执行**：网络策略在 eBPF 中执行，CPU 开销最小，性能高，每个 TAP 拥有独立的策略 trie，更新互不影响。
- **可扩展 NAT**：SNAT 端口分配使用锁保护的水线池 + 冲突检测插入，避免 iptables 规则爆炸。
- **安全隔离**：TAP 间天然不可达，只能通过 eBPF 程序控制的路径通信。

![CubeVS 设计](./assets/2026-06-23-cubesandbox-network-deep-dive/04-cubevs-design.jpg)

### 3.2 网络访问控制：基于 LPM Trie 的 CIDR 策略 IP 过滤

CubeVS 在 eBPF 内核态实现 per-sandbox 的出站 IP 策略，使用 **LPM（Longest Prefix Match）Trie** 数据结构进行 CIDR 匹配，**per-device 设计**意味着更新一个沙箱的策略不需要遍历或锁定其他沙箱的 map。

**评估顺序：**

优先级：**allow > deny > default-allow**，可以设置全拒绝（`0.0.0.0/0`）再用 allow 规则逐条放开的细粒度控制。

**默认策略**

CubeVS 默认允许沙箱访问 internet，但阻止沙箱访问以下网段以确保沙箱无法访问宿主机内网，但可以通过 allow_out 放通，也可通过指定策略 `allow_internet_access=False` 阻止沙箱访问 internet，相当于 deny_out 中添加了全拒绝（`0.0.0.0/0`）策略。

| CIDR | 说明 |
|---|---|
| `10.0.0.0/8` | 私有网络 A 类 |
| `127.0.0.0/8` | 回环地址 |
| `169.254.0.0/16` | 链路本地（沙箱内部网关所在段） |
| `172.16.0.0/12` | 私有网络 B 类 |
| `192.168.0.0/16` | 私有网络 C 类 |

### 3.3 网络访问控制：基于 LPM Trie 的 DNS 报文域名策略过滤

IP 层策略只能基于目标 IP 做 CIDR 匹配，然而 AI Agent 的沙箱网络访问通常以域名为目标（如 `api.openai.com`），且同一域名可能解析到多个不同的 IP，CubeVS 通过 **DNS 策略引擎**实现域名维度的精细控制，CubeVS 在 eBPF 层拦截 DNS 查询（UDP 端口 53），获取查询域名，如果在 allow_out 中放通了，则创建域名追踪记录，在 CubeVS 接收到 DNS 响应报文时，如果匹配上该追踪记录，则将响应报文中的 IP 地址添加到 allow_out 中，以放通后续针对该域名的访问流量，并设置 TTL 以确保删除 allow_out 中陈旧的 IP。

- 域名只能在 allow_out 中放通，不能在 deny_out 中配置。
- 为达到只放通指定域名的流量，建议 deny_out 中配置全拒绝策略（`0.0.0.0/0`）。
- allow_out 中配置了域名时，Cube 会自动将模版配置的 DNS server IP 添加到 allow_out 中。
- 限制：该能力依赖沙箱通过 UDP port 53 的 DNS 查询获取域名 IP 后发起访问，而不是其它方式，如配置 hosts。

#### 与 L7 策略的协同

DNS 域名过滤策略在 eBPF 内核层提供域名维度的控制，CubeEgress 在 L7 层提供更精细控制，包括 http/https、sni、host、path、method 的精确控制。

在沙箱 network 配置中配置了 rules 策略时，Cube 会自动将其中的 host、sni 在 allow_out 中放通域名，并标记对该域名的访问流量将上送到 CubeEgress 进行 L7 层处理，如凭证托管、访问审计，或 L7 层拒绝访问。

- **eBPF DNS 策略**：快速拒绝已知恶意域名，减少到达 L7 代理的流量。
- **CubeEgress L7 策略**：精细的域名通配（`*.example.com`）、路径前缀（`/v1/*`）、HTTP 方法等条件匹配。

## 4. 沙箱网络入向网关：CubeProxy

CubeProxy 是沙箱的**入向流量网关**，负责将外部请求路由到正确的沙箱实例，基于 OpenResty（Nginx + Lua）实现，采用 Host 头路由，兼容通配符 DNS（`*.sandbox.cube.app`），支持 TLS SNI，易于 DNS 配置。

**格式**：`{port}-{sandbox_id}.{domain}`

CubeProxy 通过 Redis 查询沙箱在宿主机上的 portmapping 信息，即根据 `(container_port, sandbox_id)` 获取 `(HostIP, mapping-port)`，将请求转发到对应母机。

## 5. 应用层安全增强：CubeEgress

### 5.1 设计定位

CubeEgress 是沙箱的**出向安全网关**，在 L7 层对 HTTP/HTTPS 流量实施深度检查。它是对 CubeVS 内核态策略的**用户态增强层**，解决内核层无法处理的域名匹配、凭证注入、内容审计等需求。

核心能力矩阵：

| 能力 | 说明 |
|---|---|
| HTTPS 透明拦截 | MITM 动态证书 + TLS 终止 |
| 域名/路径/方法匹配 | L7 策略引擎，first-match-wins |
| 凭证注入 | 代理层自动注入，Agent 不接触凭证 |
| 数据脱敏 | 审计日志中敏感字段替换 |
| 访问审计 | JSONL 格式结构化日志 |
| 请求过滤 | 允许或拒绝请求 |

限制：该能力依赖沙箱通过 UDP port 53 的 DNS 查询获取域名 IP 后发起访问，而不是其它方式，如配置 hosts。

### 5.2 按需引流：eBPF 到 L7 代理的流量调度

不是所有流量都需要 L7 检查，只有访问 rules 中配置了规则的域名的 http/https 流量才会通过 CubeVS **按需引流**到 CubeDev 设备，从而进入 kernel Stack，再结合 iptables 的 TProxy 机制和用户态 socket 设置 `IP_TRANSPARENT` 选项（当前 nginx/openresty 均不支持，Cube 提供了 patch，也提供了打过 patch 的 openresty 容器镜像），将报文透明转发到 OpenResty 的用户态 socket 接收，并保留沙箱访问的原始目的 IP。

![CubeEgress 按需引流](./assets/2026-06-23-cubesandbox-network-deep-dive/05-cubeegress-traffic.jpg)

### 5.3 HTTPS 透明拦截：动态证书生成

CubeEgress 通过 MITM（Man-in-the-Middle）代理实现对 HTTPS 流量的深度检查，核心机制是**动态证书生成**：

#### 证书架构

- **ECDSA P-256**：叶证书使用椭圆曲线，比 RSA 更快更小。
- **7 天 TTL**：短期有效，降低泄露影响。
- **缓存锁**：lua-resty-lock 防止同一 SNI 并发生成。
- **CA 信任注入**：在制作模版时默认将 Root CA 证书注入沙箱，对 Agent 透明。

### 5.4 L7 策略引擎

沙箱的 CubeEgress 策略可以包含多个 rule，每个 rule 又包含了一个 match 和 action，顺序匹配，第一个匹配的 rule 生效，不再匹配后续 rule，未匹配任何规则的请求将默认拒绝。

- match 的匹配语义：

| 字段 | 格式 | 语义 |
|---|---|---|
| sni | `*.example.com` | 精确匹配或后缀 `*.` 通配（仅匹配子域名） |
| host | `api.example.com` | 精确匹配或后缀 `*.` 通配（忽略 `:port`） |
| method | `{"GET", "POST"}` | 数组，任一匹配即通过 |
| path | `/v1/*` | 精确匹配或尾缀 `*` 前缀匹配 |
| scheme | `http` / `https` | 精确匹配 |

- action 的执行语义：allow / deny / audit / inject

### 5.5 凭证托管与注入

AI Agent 沙箱**不应直接持有 API Key/Token**，CubeEgress 的凭证注入机制让沙箱发送不含凭证的请求，由沙箱外的 CubeEgress 代理层自动注入：

凭证注入受到多道检查以保护凭证的安全，任一失败则**放弃注入**（请求仍可继续，但不携带凭证）：

| 检查内容 |
|---|
| 请求必须是 HTTPS |
| SNI 匹配规则 |
| Host 头必须与 SNI 一致 |
| 上游证书验证通过 |
| 策略授权放通 |

### 5.6 访问审计

每条网络请求（无论允许还是拒绝）都生成 JSONL（JSON Lines）格式的结构化审计记录，审计日志中的敏感字段**绝不记录原始值**，通过多层脱敏策略保护：

| Header 类型 | 脱敏策略 | 示例 |
|---|---|---|
| Authorization / Proxy-Authorization | 保留认证方案，脱敏值 | `Bearer <redacted:Bearer>` |
| Cookie / Set-Cookie | 保留 Cookie 名，脱敏值 | `<redacted; names=session_id,theme>` |
| X-Api-Key | 完全脱敏 | `<redacted>` |
| X-Auth-Token / Token | 完全脱敏 | `<redacted>` |
| 含 token/secret/key/password/auth 的 Header | 完全脱敏 | `<redacted>` |

### 5.7 CubeEgress 能力扩展

CubeEgress 是基于 OpenResty 实现的 http/https 透明代理，当前实现了基本的请求过滤/凭证托管/访问审计的能力，如果需要更深入的检测处理或其它能力，可以修改或新增 lua 脚本轻松扩展，具体可以参考社区仓库文档：<https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/guide/security-proxy.md>

## 6. 总结

CubeSandbox 网络体系通过 **"eBPF 内核态 + OpenResty 用户态"** 的组合实现了高性能的网络转发，同时通过资源分配本地化、沙箱配置归一化、资源池化等技术实现了极致的沙箱创建速度，在 AI Agent 安全沙箱这一新兴场景中实现了多项技术创新：

| 创新点 | 传统方案 | CubeSandbox 方案 | 优势 |
|---|---|---|---|
| **网络隔离** | Linux Bridge + iptables | 点对点 TAP + eBPF | 零广播开销、内核态强制 |
| **策略执行** | iptables 规则链 | LPM Trie + eBPF | O(1) 查询、per-sandbox 隔离 |
| **域名过滤** | 应用层代理 | 内核态 ebpf | 高性能转发 |
| **HTTPS 检查** | 手动配置证书 | 动态证书生成（ECDSA P-256） | 零预配置、自动管理 |
| **凭证管理** | 环境变量/配置文件 | 代理层注入 + 多道安全检查 | Agent 沙箱不接触凭证 |
| **审计日志** | 文本格式、需解析 | JSONL 结构化 + 自动脱敏 | 合规友好、可直接分析 |

欢迎关注 CubeSandbox 项目：<https://github.com/TencentCloud/CubeSandbox>
