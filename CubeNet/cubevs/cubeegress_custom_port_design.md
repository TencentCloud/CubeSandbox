# CubeEgress 自定义端口支持 — 完整方案

本文档整理 CubeEgress 支持「L7 规则自定义目的端口（任意 `host:port` 上的 HTTP/HTTPS
拦截与注入/审计）」的**端到端完整方案**，覆盖从 API 配置模型、服务端校验、传输、到
CubeEgress 运行时（Lua + iptables）与数据面 eBPF（`CubeNet/cubevs`，`allow_out_v3`）的
全部层次。

涉及的关键提交：

- `43fa0bf`（当前 `master` 基线）—— 分支已于 2026-07-29 rebase 到此；原分叉点 `68d9165`
  上磁盘仍是旧 `allow_out_v2` / `dns_allow` / `dns_query_track` 布局。
- `ff3aea3` / `fc9acac`（`CubeEgress rules support port configuration`）—— 本特性的全栈落地，
  跨 `CubeAPI` / `CubeMaster` / `network-agent` / `CubeEgress`(Lua+iptables) / `Cubelet` /
  `CubeNet/cubevs` 多个组件。
- `2fdcbc6`（`test(cubenet): cover LPM fallback and expiry`）—— 数据面内核级 BPF 回归测试。

> **当前状态（`L7_support_portconfig` 已推送 origin，领先 `master` 41 个提交）**
>
> 本特性已经过一轮完整代码评审（5 路并行评审 + 数据面专项复核），发现的 **3 个 Blocker、
> 8 个 Major 已全部修复或立项**，Minor 与待决项亦已处理：
>
> - **Blocker**：network-agent recover 测试 fixture（`4023404`）、CubeAPI 测试编译 `E0063`
>   （`091a7bb`）、eBPF 普通域名 allow 未学成 `/32` 的功能回归（`c227ef2` + 数据面回归测试
>   `3e725b7`）。
> - **Major**：M1 主机/CIDR 归一化端口丢失与 scheme 冲突绕过（`4cc7f77`）、M2 拒绝 subnet
>   host（`a3c87de`）、M3 条目数校验按 `/48` 展开计数（`fcf2886`）、M4 迁移崩溃安全
>   （`c277758`）、M5 iptables grep 大小写幂等（`850e2cc`）、M7 e2e 断言收紧（`34a94e8`）、
>   M8 恢复 master 的 SNI+Host 双投（`e929081`）；M6「`(hostname)` 校验域 vs `(ip,port)`
>   数据面键」冲突解析规则见 §4.2.1 与 §10。
> - **顺带修复**：`dns_learn_response_ip` 循环不展开的 verifier 脆弱性（`bb8d489`）、
>   `make test-lua` 接入 `port_scheme_extra_test.lua`（`a09edf2`）。
> - **后续清理**：48B `net_policy_value_v2`（本需求开发中途被放弃的过渡格式）已由 `9244355`
>   还原为 16B 并删除其迁移/stale 死代码；M4 的 stale 备份崩溃安全（`c277758`）随之移除。
> - **Minor / 待决项**：`lpmKeyV3` ABI 断言、`dump.go` 端口主机序、死代码清理（`15c093c`）；
>   `recover()` 单沙箱 skip-and-log、`state_store.Save` 原子写（`f679d3d`）；SDK scheme 小写
>   归一、proto 注释自指、CubeMaster 深拷贝 `Port`（`9d5d66e`）；迁移期 legacy L7 subnet 规则
>   丢弃并告警（`e9e9463`）。
> - **平滑升级（第二轮）**：`dns_allow` 真改名为 `dns_allow_v2`（BPF 层，与 `allow_out_v3`
>   同策略，`e1c8204`）；迁移测试补齐——inner-map 值变换（legacy/current，`a91f026`）、
>   bpffs outer-map（`193ed22`）、失败回滚保留 legacy pin（`4734748`）。
> - **L7 mark 安装期可配置**：eBPF `const volatile` 全局 + `rewriteConstants` 注入
>   （`170c102`）、network-agent 读取 `/etc/cubeegress/l7-marks.conf`（`8e09380`）、
>   `install.sh` 由 env 生成该配置并校验（`d0d584a`）、部署文档（`a8304b9`）；
>   生成物 `network_agent.pb.go` 回同步（`1954aa6`）。数据面与 iptables 同源读取，
>   单侧改动即双侧一致。
> - **现网验证（2026-07-28）**：one-click dist `7480d03`（构建自 rebase 前同名提交，rebase 后
>   映射为 `a8304b9`）部署上 `test_l7_custom_port_e2e.py` 2/2 通过（自定义 HTTP 端口注入、
>   HTTPS 自定义端口拒绝 403、HTTPS 自定义端口放行 200）；同机 mark 覆盖
>   `0xA0000000/0xB0000000`（mask `0xF0000000`）在 eBPF 与 iptables 双侧同步生效
>   （TPROXY 计数器持续命中），安装期可配置链路一并得到现网验证。
> - **示例与文档**：自包含示例 `network_l7_custom_port_echo.py`（5 规则 6 探针，现网 6/6，
>   `d01f222`）取代占位符示例；文档同步（`709a8ac`、`3aeb4d2`）。
> - **遗留连接排空**：`allow_out_v3` 条目老化后不再 RESET 已建立的默认端口（80/443）L7
>   连接——非 SYN 包会话缺失时经 `bpf_skc_lookup_tcp` 确认代理 socket 仍 ESTABLISHED 则
>   重新打标引流（`c72fc75`）；自定义端口连接有意排除，遵守当前策略。
> - **评审（第三轮，2026-07-29）**：CLI `cloneCubeNetworkConfig` 丢弃 `Rules` /
>   `AllowPublicTraffic`（分叉点遗留，master 已由 `2a18402` 修复为 `DeepCopy` 路线，本分支
>   补回归测试 `cd2335c`）；recover reconcile 失败的沙箱被清理循环误删持久化状态（改为
>   保留现场、下次重启重试、清理错误不再中止启动，`7780204`）；shell 侧 marks 校验改算术
>   比较与 Go `resolveL7Marks` 三方对齐（`dcbed23`）。
> - **Rebase（2026-07-29）**：分支重放到新 `master`（`43fa0bf`），clone 路线与 master 的
>   `DeepCopy` 对齐，并为 `types.EgressRule.DeepCopy` 补 `Match.Port` 深拷贝（master 该方法
>   早于 port 字段存在，直接采用会丢弃端口绑定规则）。全部提交 hash 随之更新，本文引用
>   已同步。
>
> **已知遗留**：`TestTCPUpdateSession*` 此前在本环境内核（`6.6.69-cube`）被 BPF verifier
> 拒绝——`tcp_conntracks[dir][index][old_state]` 三维数组访问（边界已检查、访问安全，
> 系 verifier 无法跟踪多维变量索引，非真实越界），测试每次自行跳过。已由 `fbee460`
> 根治：`get_conntrack_index` 加 `__always_inline` 后 clang 内联并对常量 flag 输入做
> 常量传播，折叠掉 verifier 需推理的一维，三个测试现已在该内核**真正加载并通过**，
> cubevs 套件本内核首次全绿。其余 CubeMaster 失败（`localcache` panic、缺 `conf.yaml`、
> 依赖 registry/MySQL 的测试）均为预先存在/环境性，与本分支无关。

---

## 0. 设计原则

1. **改 eBPF map 定义则新建兼容版本**：`allow_out_v2` 不被原地改写，新增 `allow_out_v3`，
   迁移逻辑把旧数据无损搬入（见 §6 / §7）。
2. **任意端口靠 `skb->mark` 选择，而非 `dport`**：iptables TPROXY 匹配的是 mvmtap 在
   sandbox tap 写入的 fwmark，因此用户把 L7 挂到 `tcp/3000` 之类任意端口时，无需教
   iptables 知道这个端口映射。
3. **逐端口 `(host, port, scheme)` 语义**：`port` + `scheme` 共同钉死 CubeEgress 要拦截的
   `(host, port)` 元组，且同一 `(host, port)` 所有规则必须对该端口的 scheme 达成一致。
4. **向后兼容默认 `{80/http, 443/https}`**：不配置 `port`/`scheme` 的规则保持经典行为，
   新旧规则可混用。

---

## 1. 配置模型（API schema）

`EgressRuleMatch` 新增可选端口字段，跨多份定义保持 lockstep：

- `CubeAPI/src/models/mod.rs`（`EgressRuleMatch.port: Option<i32>`）
- `CubeAPI/src/services/sandboxes.rs`（`map_egress_rule` 透传 `port`）
- `Cubelet/api/services/cubebox/v1/cubebox.proto`（`optional int32 port = 8`）
- `network-agent/api/v1/network_agent.proto` 与 `Cubelet/pkg/networkagentclient/pb/network_agent.proto`
  （`optional int32 port = 8`，注释要求与 `CubeNet/src/cubevs.h` / Lua 保持同步）

### 端口/协议语义（`CubeEgress/lua/port_scheme.lua` 的 `expand`）

| 配置 | 展开结果 |
|------|----------|
| `port`/`scheme` 均 nil | 默认 `{80/http, 443/https}` |
| 仅 `scheme=http` | `{80/http}` |
| 仅 `scheme=https` | `{443/https}` |
| `port` + `scheme` 都设 | 精确 `(port, scheme)` 元组 |
| `port` 设但 `scheme` 缺 | **非法**（校验报错 `port requires scheme`） |

约束（来自 `normalize_port` / `normalize_scheme`）：

- `port ∈ [1, 65535]` 且为整数；`scheme ∈ {http, https}`（大小写/空白归一化）。
- 同一 `(host, port)` 跨规则必须 scheme 一致（见 §2 服务端强校验）。
- 每 host 最多 **8** 个 `(port, scheme)` 元组（`MAX_L7_PORTS_PER_HOST = 8`）。

---

## 2. 模型定义与透传（CubeMaster）+ 运行时强校验（CubeEgress）

CubeMaster 这一层**只定义 `Port` 字段并原样透传**到 `cubebox.proto` / `network-agent`，
不在 Go 侧做端口/scheme 冲突校验；`types.go` 的文档注释描述的是**契约**——
「同一 `(host, port)` 的所有规则必须就 scheme 达成一致，否则整条 policy 被拒」。
该契约的**硬校验在 CubeEgress 加载策略时**执行（见 §4.2）。

- `CubeMaster/pkg/service/sandbox/types/types.go`：`EgressRuleMatch` 增加 `Port *int`，并文档化
  上述语义（含上面的整策略原子性契约）。
- `CubeMaster/pkg/templatecenter/template_request.go`：`cloneEgressRule` 透传 `Port`
  （新增 `cloneIntPtr`）。
- `CubeMaster/pkg/service/sandbox/util.go`：`mapEgressRuleMatch` 把 `Port` 映射到 `cubebox.proto`
  的 `EgressRuleMatch.Port`。
- **聚合 key**：当两个 identity（`host` / `sni`）都提供时以 **Host** 为聚合 key，否则用 **SNI**；
  请求时两者都参与匹配（`CubeEgress/lua/policy.lua` 的 `validate_match_tuples`）。
- **整策略原子性**：端口/scheme 冲突的校验在 CubeEgress 策略加载时触发（`validate_match_tuples`
  返回 `conflicts for host %q port %d: %s vs %s`），导致**整条 policy 校验失败**而非部分生效。

---

## 3. 传输（network-agent proto）

`EgressRuleMatch.port` 经 `network-agent` 的两份 `.proto`（`network-agent/api/v1/...
` 与 `Cubelet/pkg/networkagentclient/pb/...`，内容一致）从 CubeMaster 透传到 Cubelet，
再由 Cubelet 的 network-agent client（`types.go` 增加对应字段）下发给 CubeEgress worker。
proto 注释明确：**`port` 设了则 `scheme` 必设**，且必须与 `CubeNet/src/cubevs.h` 的
`L7_SCHEME_*` 与 Lua `port_scheme` 保持 lockstep。

---

## 4. CubeEgress 运行时（Lua）

### 4.1 `CubeEgress/lua/port_scheme.lua`（新增）—— 语义与匹配原语

- `normalize_scheme` / `normalize_port`：输入归一化与范围校验。
- `expand(port, scheme)`：按 §1 表展开为有效 `(port, scheme)` 元组列表（校验报错透出到调用方）。
- `matches(port, scheme, dst_port, request_scheme)`：请求时判定——先 `expand` 得元组集合，再比对
  当前请求的 `dst_port`（由 TPROXY 保留的**原始目的端口**）与 `scheme`。

### 4.2 `CubeEgress/lua/policy.lua` —— 校验

- 新增 `MAX_L7_PORTS_PER_HOST = 8`（与 `cubevs.h` 一致）与 `normalize_identity`。
- `validate_match_tuples`：调 `port_scheme.expand` → 以 identity 为 key 累加 `ports` 集合；
  - 同 `port` 已有不同 `scheme` → 报错（`conflicts for host ... port ...`）；
  - 单 host 超过 8 个元组 → 报错（`exceeds 8 L7 port tuples`）。
- `validate_policy` 在遍历 `rules[]` 时对每条 `match` 调用上述校验，保证整策略原子性。

#### 4.2.1 冲突解析规则：`(hostname)` 校验域 vs `(ip, port)` 数据面键

`validate_match_tuples` 以**主机名字符串**为桶（`host` 缺省回退 `sni`），只能保证「同一主机名同一
port 的 scheme 一致」。它**无法**发现「两个**不同**主机名解析到同一 `(ip, port)` 却用不同 scheme」的
冲突——那需要在策略加载期做 DNS 解析，既脆弱又常离线不可行，故不在校验范围内。

而数据面 `allow_out_v3` 以 `(ip, port)/48` 为键，**每个 `(ip, port)` 只能存一个 scheme**。因此：

- **解析规则**：对解析到同一 `(ip, port)` 的多个主机名，数据面按**最后一次写入生效**裁决——scheme 取
  最近学成（`dns_learn_response_ip`）/ 落表（`populateAllowOutInnerMap`）的那条。`flags` 按位或仅在
  **精确同键**（`key_prefixlen` 相同）时发生；`expires_at_ns` 同键时**静态赢**：任一侧为静态（0）结果
  即为 0（学习遇同键静态保持 0；静态落表遇同键学习项覆盖为 0——保留学习 TTL 会让 reaper 在旧 TTL
  到时后删表，静态裁决凭空消失），两侧皆学习则新 TTL 续期。此前 LPM 最长前缀查找把「同键」放大成
  「任意覆盖项」，静态 CIDR 的零过期会传染给被覆盖的学习项（永不老化）——已由 `key_prefixlen` 精确
  匹配 + 静态赢修正（回归测试见 §9）。同键合并的裁决结果仍**不确定**（取决于 DNS 响应到达顺序）。
- **作者责任**：凡解析到同一 `(ip, port)` 的主机名，其 scheme **必须一致**。Lua 加载期无法代查，
  须策略作者保证。
- **违反后果**：该 `(ip, port)` 的流量被引到「最后学成」scheme 对应的 listener（8080/8443），
  另一主机名/scheme 的请求将在该 listener 上按 `(host, port, scheme)` 匹配**失败**。

### 4.3 `CubeEgress/lua/access_phase.lua` —— 请求匹配

- `rule_matches` 用 `port_scheme.matches(m.port, m.scheme, ctx.dst_port, ctx.scheme)` 取代旧的
  仅 scheme 比较。`port`/`scheme` 缺省表示「向后兼容的默认端口集」，而**不是**对该 host 上
  任意自定义端口的通配。
- `build_ctx` 新增 `dst_port = tonumber(ngx.var.server_port)`：在 **IP_TRANSPARENT** listener 下，
  nginx 的 `$server_port` 经 `getsockname()` 报告的是**原始目的端口**（TPROXY 保留），而非我们
  bind 的 8080/8443。这是 `match.port` 约束能在自定义端口上生效的前提。
- `decide()` 把 `dst_port` 一并传入决策。

---

## 5. iptables / 策略路由（`CubeEgress/scripts/cube-proxy-iptables-init.sh`）—— 关键转变

### 旧模型（只能 80/443）

按 `iif cube-dev` + `tcp dport 80/443` 选择，无 fwmark 参与。

### 新模型（任意端口）

按 **`skb->mark`** 选择——mvmtap 在 sandbox tap 读出 `allow_out_v3` 的
`(ip, port) → scheme` 映射（host→ip 由 DNS 学习层解析），写入 `CUBE_L7_MARK_HTTP=0xCE010000` /
`CUBE_L7_MARK_HTTPS=0xCE020000`（mask `0xFFFF0000`，三者均为**默认值**），本脚本据此把包
引入 8080（HTTP listener）或 8443（HTTPS listener），**与原目的端口无关**。

> 三个 mark 值**安装期可配置**：`install.sh` 把 env（`CUBE_L7_MARK_HTTP/HTTPS/MASK`）写入
> `/etc/cubeegress/l7-marks.conf`（校验 http≠https、取值不越出 mask，非法拒绝安装）；
> network-agent（注入 eBPF `const volatile` 全局）与本脚本（iptables/ip rule）**同源读取**
> 该文件，故两侧始终一致。下文规则示例均为默认值。

具体规则变化：

```sh
# 旧
iptables -t mangle -A TPROXY -i cube-dev（即 $INGRESS_IFACE） -p tcp --dport 80  -j TPROXY --on-port 8080
iptables -t mangle -A TPROXY -i cube-dev（即 $INGRESS_IFACE） -p tcp --dport 443 -j TPROXY --on-port 8443
# 新
iptables -t mangle -A TPROXY -i cube-dev -p tcp \
    -m mark --mark 0xCE010000/0xFFFF0000 -j TPROXY --on-port 8080
iptables -t mangle -A TPROXY -i cube-dev -p tcp \
    -m mark --mark 0xCE020000/0xFFFF0000 -j TPROXY --on-port 8443
```

策略路由同样由「`iif` + `dport`」改为「`fwmark`」：

```sh
ip rule add fwmark 0xCE010000/0xFFFF0000 table 100   # 取代旧的 iif cube-dev ipproto tcp dport 80
ip rule add fwmark 0xCE020000/0xFFFF0000 table 100
```

- **高位 16 bit 为 cube 自有**（`0xCE..`），低位 16 bit 留给用户其他 host 级 fwmark，由 mask 隔离。
- 新增 `remove_legacy_dport_routing`：升级时清理旧 `iif cube-dev ipproto tcp dport 80/443`
  ip rule（幂等），避免两套选择器并存。
- 脚本幂等（`install_*` / `remove_*` / `show_status`），可安全重跑。

---

## 6. 数据面 eBPF（CubeNet/cubevs，`allow_out_v3`）

> 本层细节见同目录 `allow_out_v3_design.md`，此处仅列与自定义端口直接相关的要点。

- **map 拓扑**：`allow_out_v3`（`BPF_MAP_TYPE_HASH_OF_MAPS`）→ inner `LPM_TRIE`。
  - key `lpm_key_v3`（12B）= `prefixlen`(4B) + `ip`(4B) + `port`(2B) + `_pad`(2B)；
    `prefixlen=48` 即精确 `(ip, port)`，`32` 即仅 IP，`<32` 即子网。
  - value `net_policy_value_v3`（16B）= `ExpiresAtNS` + `Flags` + `Scheme` + `Reserved`；
    scheme 在**插入时**解析落定，不再需要 8 元组数组。
- **数据面解析**（`CubeNet/src/session.h` `classify_egress_flow`，由 `mvmtap.bpf.c` 的
  `do_tcp_nat` 在新建流时调用 / `udp.h`·`icmp.h` 的 `create_*_sessions` 在新建会话时调用）：
  统一裁决出向流，**一次**对 `(ip, dport)/48` 做 LPM 查找。LPM 的「最长前缀回退」会自动把
  精确 `(ip, port)` 回退到 `/32` 或子网规则；命中有效 `L7_REQUIRED` 条目即得 `scheme`
  （`FLOW_HTTP`/`FLOW_HTTPS`），其它命中（含回退到的普通 allow）一律 `FLOW_SNAT`；
  仅当该次查找未命中、或命中的 allow 已过期，才去查 `deny_out` 决定 `FLOW_REJECT`，
  否则默认 `FLOW_SNAT`。**只查一次 inner LPM trie** 即可覆盖「精确 L7 / 回退普通放行 /
  default 放行」三态，无需为普通放行再查一次。

  **注意**：旧实现 `l7_scheme_for_flow` 与 `session_policy_allowed` 已被合并。原
  `session_policy_allowed` 用硬编码 `/32` key，无法匹配 DNS 下发的 `(ip,port)/48` L7 条目，
  导致已授权的 L7 流被 `deny_out 0.0.0.0/0` 误杀；合并后 `/48` L7 查找优先于 deny，
  修复该回归。
- **scheme → `skb->mark`**（`mvmtap.bpf.c` 的 `do_tcp_nat`）：`L7_SCHEME_HTTP → 0xCE010000`，
  `L7_SCHEME_HTTPS → 0xCE020000`，与 §5 的 iptables 标记完全对应。
- **用户态落表**（`netpolicy.go` `populateAllowOutInnerMap`）：L7 规则为每个 `(port, scheme)` 生成一条
  `/48` 条目（未显式给端口则按默认 `{80, 443}` 展开）；普通 allow 为单条 `/32`（`port=0`、`scheme=NONE`）。
- **DNS 学习**：`dns_allow_v2` 的 `dns_allow_value.ports[]` 在域名学成时继承 `(port, scheme)`，响应处理
  据此回填 `allow_out_v3`；`dns_query_track_value` 同样带 `ports[]` 以便免二次查找。

---

## 7. 兼容与平滑升级

### 7.1 `CubeNet/cubevs` 旧 pin 清理（本次特性在 `migration.go` 落地）

- 迁移成功后通过 `removePinnedMap` 删除旧 pin：`allow_out_v2` / `allow_out`（优先 v2，回退 `allow_out`）
  与 `dns_allow`。
- 安全性：inner LPM_TRIE 仅存在于外层 HashOfMaps 内（经 `outerMap.Put`，未单独 pin），
  故 `os.Remove(pinPath(name))` 为单文件 unlink 即可，内核在所有 fd 关闭后自动回收 inner map。
- 仅**迁移成功时**删除；失败则保留旧 pin 供下次重启重试，不会留下半迁移状态。
- `dns_query_track` 不迁移：`Init` 无条件删除旧 pin 后由 `loadObject` 以新布局重新 pin（毫秒级在途
  查询状态，重启可自愈）。

### 7.2 启动顺序（`cubevs.Init`，`miscs.go`）

1. `persistentPolicyGenerationExists`（判定新 pin 是否已存在）。
2. 删 `tungrp_to_tuns` 与 `dns_query_track` 旧 pin。
3. `loadObject` ×3（localgw/mvmtap/nodenic）以新名字 pin 出空 `allow_out_v3`/`dns_allow_v2`/`dns_query_track`。
4. `migratePersistentPolicyMaps`（灌入旧数据 + 清理旧 pin，见 7.1）。
5. attach TC filter。

> 注：早期版本曾在第 1 步捕获「48B 旧布局 `allow_out_v3`」并在第 6 步 `replayStashedAllowOutV3`
> 回放（含崩溃安全备份）。该 48B `net_policy_value_v2` 是本需求**开发中途被放弃的方案**，真实部署
> 不存在，已由提交 `9244355` 整体移除（`net_policy_value_v2` 还原为 16B，迁移只认 16B 旧格式，
> 48B inner map 直接判为不支持）。

### 7.3 iptables 侧

- `remove_legacy_dport_routing` 升级时清理旧的 `iif + dport 80/443` ip rule，与 §5 新 fwmark 选择器不冲突。

### 7.4 向后兼容

- 未配置 `port`/`scheme` 的规则保持默认 `{80/443}` 行为，新旧规则可混用。

---

## 8. 字节序与 ABI 守卫

- **网络字节序强制**：`lpm_key_v3` 的 `ip`/`port` 必须以网络字节序填充（与 `iphdr->daddr` /
  `tcphdr->dest` 一致），否则精确 `(ip, port)` 匹配静默失败（`cubevs.h:170`）。
- **双端静态断言**：`cubevs.h` 的 `_()` 校验 `lpm_key_v3==12`、`net_policy_value_v3==16`、
  `dns_allow_value==40` 等；`cubevs.go` 的 `_()` 在 Go 侧镜像校验同一组尺寸，任一侧漂移即编译失败。
- **proto/lua lockstep**：`network_agent.proto`（server + client 两份）、`cubebox.proto`、`port_scheme.lua`、
  `cubevs.h` 的 `L7_SCHEME_*` 必须保持 `(port, scheme)` 语义同步。
- **eBPF 重新生成**：`go generate ./...` 重编译 `bpfel.go`。

---

## 9. 测试与验证

| 层 | 新增/改动 | 说明 |
|----|-----------|------|
| Lua | `CubeEgress/lua/port_scheme.lua`（新）、`port_scheme_test.lua`（新） | `expand` / `matches` 单测 |
| iptables | `CubeEgress/tests/cube-proxy-iptables-init_test.sh`（新） | fwmark 选择器验证 |
| policy | `CubeEgress/lua/policy.lua` | `validate_match_tuples` 冲突/超限校验 |
| Go/eBPF | `CubeNet/cubevs/custom_port_test.go`、`CubeNet/cubevs/egress_policy_test.go` | v3 ABI 守卫、内核 BPF LPM 回退与过期回归 |
| Go/DNS 学成 | `CubeNet/cubevs/dns_learn_test.go`（+ `CubeNet/src/dns_learn_test.bpf.c`） | 普通 `/32`、L7 默认 `{80,443}`、L7 显式端口的数据面回填回归；同键合并三例（覆盖静态 CIDR 不传染、同键静态保留、同键学习 TTL 续期 + flags 并入 + scheme 覆盖，均经变异验证） |
| Go/迁移 | `CubeNet/cubevs/migration_test.go` | `dns_allow` inner-map 值变换（legacy/current）、bpffs outer-map 迁移、失败回滚保留 legacy pin；`TestSkipUnsupportedL7Subnet` |
| Go/L7 mark | `CubeNet/cubevs/l7_mark_test.go`（+ `CubeNet/src/l7_mark_test.bpf.c`）、`network-agent .../local_service_test.go::TestLoadL7MarksConfig` | `resolveL7Marks` 校验、配置注入数据面印记、`l7-marks.conf` 解析（缺省/覆盖/非法） |
| e2e(py) | `sdk/python/tests/test_l7_custom_port_e2e.py`（新）、`test_policy.py` | 自定义端口拦截/注入/审计端到端 |
| 示例 | `examples/code-sandbox-quickstart/network_l7_custom_port_echo.py`（新） | 单沙箱全规则矩阵示范（5 规则 / 6 探针）：scheme-only 默认 :80/:443、裸 host 规则展开默认集 `{80/http, 443/https}`、显式 `(port,scheme)` 自定义 :18080/http 与 :1012/https；本地 echo（自动探测 bridge IP）+ 公共回显端点断言注入 marker 到达；约束见 §10 |

内核级 BPF 回归（提交 `2fdcbc6`，`CubeNet/cubevs/egress_policy_test.go`）：

- `TestClassifyEgressFlowLPMFallback`：`/48` 查询回退到 `/32` IP 规则、`/24` 子网规则均得
  `FLOW_SNAT`；以 `deny_out 0.0.0.0/0` 兜底证明结果来自 LPM allow 回退而非默认放行。
- `TestClassifyEgressFlowExpiredAllow`：过期 `/48` L7 allow（带 `deny_out`）→ `FLOW_REJECT`；
  过期回退 `/32` allow（带 `deny_out`）→ `FLOW_REJECT`；锁定「过期最长前缀命中不被
  当作放行、但合并后直接进 deny」的当前语义。
- `TestClassifyEgressFlowExactL7Match`：精确 `/48` L7 allow
  （`192.0.2.20:8443` HTTPS）+ `deny_out 0.0.0.0/0` 兜底 → 同端口 `FLOW_HTTPS`、
  异端口（443）回退 deny → `FLOW_REJECT`；直接锁定「单次 `/48` 查找 + LPM 不跨端口回退」语义。

端到端验证（`CubeNet/cubevs`）：`go build` / `go vet` / `go test` 全绿，`gofmt` 干净。
`test_policy.py` 的用例与 `port_scheme_test.lua` 覆盖自定义端口拦截/注入/审计。

现网验证（2026-07-28，one-click dist `7480d03`，API `127.0.0.1:3000`）：

- `CUBE_E2E=1 CUBE_L7_E2E_HTTP_TARGET_HOST=172.18.0.1 pytest tests/test_l7_custom_port_e2e.py`
  → **2/2 通过**：自定义 HTTP 端口注入（本地 echo 目标收到注入头）、HTTPS 自定义端口拒绝
  （`:1012` → 403）、HTTPS 自定义端口放行到真实上游（`:1012` → 200，非空 body）。
- 同机 mark 覆盖 `0xA0000000/0xB0000000`（mask `0xF0000000`）经 `/etc/cubeegress/l7-marks.conf`
  在 eBPF 印记与 iptables/ip rule 双侧同步生效（TPROXY 计数器持续命中）——单一来源、双消费方
  一致性成立。
- 同机运行示例 `network_l7_custom_port_echo.py` → **6/6 探针通过**：scheme-only 默认 :80/:443、
  裸 host 规则（无 port/scheme）同规则覆盖默认集 :80+:443、自定义 `:18080`/http（本地 echo 收到
  注入 marker）、自定义 `:1012`/https（200 + 非空 body）——全规则矩阵现网验证。
- 运维提示：用 `example.com` 演示自定义端口会超时（其 8080/8443 握手后被静默丢弃，宿主机
  直连同现象），并非 CubeEgress 故障；演示成功路径请用可响应的目标（如 e2e 的本地 echo）。

---

## 10. 注意事项 / 限制

1. **每 host 最多 8 个 `(port, scheme)` 元组**（`MAX_L7_PORTS_PER_HOST`），由 value 大小有界与
   数据面可展开性决定；超限须在 Lua 校验期报错。
2. **`port` 必须配 `scheme`**，且同一 `(host, port)` 跨规则 scheme 必须一致，否则**整条 policy 被服务端/运行时拒**。
3. **自定义端口须显式声明** `port + scheme`；否则回退默认 `{80, 443}`，不会被识别为 L7 TPROXY。
4. **TPROXY 依赖 IP_TRANSPARENT listener** 以取得原始目的端口（`$server_port` 经 `getsockname` 报原 dst），
   这是 `match.port` 约束在自定义端口生效的前提。
5. **`dns_query_track` 重启丢弃在途查询**（毫秒级、可自愈，设计预期）。
6. **fwmark 高位 16 bit 默认 cube 自有**（`0xCE..`），低位留给用户其他 host 级 fwmark，由
   `CUBE_L7_MARK_MASK`（默认 `0xFFFF0000`）隔离。三个 mark 值**安装期可配置**
   （env → `install.sh` → `/etc/cubeegress/l7-marks.conf`，数据面与 iptables 同源读取）；
   覆盖时须保证 http≠https 且取值不越出 mask，否则 `install.sh` 拒绝写入。
7. **跨主机名的 `(ip, port)` scheme 冲突由作者保证**：数据面 `(ip, port)/48` 每元组仅一个 scheme；两个
   解析到同一 `(ip, port)` 的主机名若 scheme 不同，按**最后学成生效**（不确定，见 §4.2.1）。Lua 校验
   以主机名为域、无法在加载期发现该冲突（需 DNS），须策略作者保证这些主机名的 scheme 一致。
