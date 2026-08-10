# CubeEgress 自定义端口 — 测试用例规格

本文档列出「自定义端口（L7 规则 `port` + `scheme` 钉死 `(host, port, scheme)` 元组）」
在各层的**测试场景**与**覆盖状态**。图例：

- **现有** —— 已由代码库既有测试覆盖（无需新增）。
- **新增** —— 本次生成的测试文件补齐的覆盖（见文末「新增文件」）。
  （`port_scheme_test.lua` 等特性落地时新增的文件，相对「评审修复轮」记为 **现有**；相对 `master` 它们同样是本分支新增，见设计文档 §9。）
- **现网验证** —— 已在真实集群部署上由 e2e 实跑验证（见文末「现网验证记录」）。
- **缺口** —— 尚未自动化、建议后续补齐的场景。

> 状态基线说明（2026-07-28 更新）：`CubeNet`(Go) / `CubeEgress`(Lua+shell) / `sdk/python`(单测)
> 均已在本地实跑通过；两个 py e2e 文件需集群，其中 `test_l7_custom_port_e2e.py` 已在
> one-click 部署（dist `7480d03`）上实跑 **2/2 通过**（见文末「现网验证记录」），
> `test_l7_custom_port_validation_e2e.py` 负向用例仍需集群执行、未实跑。

---

## 1. API / SDK（`CubeAPI` + `cubesandbox` SDK）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| 规则带 `port` + `scheme` | 正常建规则 | 现有 | `network_l7_custom_port_echo.py` 示例 |
| 规则带 `port` 但无 `scheme` | **客户端 `ValueError`** | **新增(e2e)** | `test_l7_custom_port_validation_e2e.py::test_l7_custom_port_requires_scheme` |
| `scheme` 大小写/空白归一（`" HTTPS "`→`"https"`） | 归一化生效 | **新增(lua)** | `port_scheme_extra_test.lua`（`expand(nil," HTTPS ")`） |
| 同 `(host, port)` 不同 `scheme` 跨规则 | 整条 policy 被拒 | **新增(e2e)** | `test_l7_custom_scheme_conflict_rejected` |

## 2. CubeMaster（Go）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| `EgressRuleMatch.Port` 字段透传（`types.go`/`template_request.go`/`util.go`） | 原样下发，无强校验 | 现有(单元测试隐含) | `types.go` doc-test 由 Go 编译守护 |
| 同 `(host, port)` scheme 冲突契约 | 契约文档化，硬校验在 CubeEgress | 现有 | `types.go` 注释（执行点在 §4） |

## 3. network-agent 传输（proto）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| `EgressRuleMatch.port` server/client 两份 `.proto` 一致 | lockstep | 现有 | `network_agent.proto` ×2 |
| `port` 设则 `scheme` 必设（契约） | 文档化 | 现有 | `.proto` 注释 |

## 4. CubeEgress Lua 运行时

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| `expand(nil,nil)`→`{80/http,443/https}` | 默认端口集 | **新增(lua)** | `port_scheme_extra_test.lua` |
| `expand(nil,"http")`→`{80/http}`；`(nil,"https")`→`{443/https}` | scheme-only 展开 | **新增(lua)** | `port_scheme_extra_test.lua` |
| `expand(8080,"http")`→`{8080/http}` | 自定义端口精确展开 | **新增(lua)** | `port_scheme_extra_test.lua` |
| `expand(8080,nil)` → 报错 `port requires scheme` | port 有 scheme 缺非法 | **新增(lua)** | `port_scheme_extra_test.lua` |
| `expand(nil,"ftp")` / `port=0` / `port=70000` → 报错 | 范围/取值校验 | **新增(lua)** | `port_scheme_extra_test.lua` |
| `matches` 默认 80/443、自定义端口拒绝、scheme 不匹配 | 请求时匹配 | 现有 | `port_scheme_test.lua` |
| `matches` scheme 大小写不敏感 | `HTTPS`==`https` | **新增(lua)** | `port_scheme_extra_test.lua` |
| `rule_matches` 同 host 规则不忽略自定义端口规则 | 精确 `(host,port)` 优先 | 现有 | `port_scheme_test.lua`（`access._rule_matches`） |
| `validate_match_tuples` 同 `(host,port)` scheme 冲突 → 整策略失败 | 冲突校验 | 现有 | `port_scheme_test.lua` |
| 同 host > 8 个 `(port,scheme)` 元组 → 失败 | 预置上限 | 现有 | `port_scheme_test.lua` |
| 同 host 同端口不同 scheme → 不允许 | per-(host,port) 一致性 | 现有 | `port_scheme_test.lua` |

## 5. iptables / 策略路由（`cube-proxy-iptables-init.sh`）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| `skb->mark` 命中 `CUBE_L7_MARK_HTTP` → TPROXY 到 8080 | HTTP listener | 现有 | `cube-proxy-iptables-init_test.sh` |
| `skb->mark` 命中 `CUBE_L7_MARK_HTTPS` → TPROXY 到 8443 | HTTPS listener | 现有 | `cube-proxy-iptables-init_test.sh` |
| 任意自定义端口（如 3000）按 mark 选路，不依赖 `--dport` | 端口无关 | 现网验证 | e2e `:18080`/`:1012` 经 mark 命中 TPROXY（2026-07-28）；iptables 级定向单测仍建议补 |
| `remove_legacy_dport_routing` 升级清理旧 dport 规则 | 幂等清理 | 现有 | `cube-proxy-iptables-init_test.sh` |

## 6. eBPF 数据面（`CubeNet/cubevs`）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| `expandDefaultPortSet()` = `{80/http, 443/https}`（网络字节序） | 默认端口集 | **新增(go)** | `custom_port_test.go::TestExpandDefaultPortSet` |
| `buildV3Entries` L7 显式端口 → 每端口一条 `/48` 条目 | 精确 `(ip,port)` | **新增(go)** | `custom_port_test.go::TestBuildV3EntriesL7ExplicitPorts` |
| `buildV3Entries` L7 无显式端口 → 默认 `{80,443}` 展开 | 默认展开 | **新增(go)** | `custom_port_test.go::TestBuildV3EntriesL7DefaultExpansion` |
| `buildV3Entries` 非 L7 → 单条 `/32`（`port=0`,`scheme=NONE`） | 普通 allow | **新增(go)** | `custom_port_test.go::TestBuildV3EntriesPlainAllow` |
| `buildV3Entries` 非 L7 子网 → `prefixlen<32` | 子网 | **新增(go)** | `custom_port_test.go::TestBuildV3EntriesSubnet` |
| `ExpiresAtNS` 透传到每条展开条目 | TTL 保留 | **新增(go)** | `custom_port_test.go::TestBuildV3EntriesExpiryCopied` |
| `lpm_key_v3.Port` 为**网络字节序** | 精确匹配不静默失败 | **新增(go)** | `custom_port_test.go::TestLpmKeyV3NetworkByteOrder` |
| `buildL7Plan` 默认规则→`PortCount=0`、显式端口→`PortCount=N` | 端口聚合正确 | 现有 | `netpolicy_test.go` |
| `buildL7Plan` scheme 冲突 / >8 元组 / 同 host 独立 | 校验 | 现有 | `netpolicy_test.go` |
| `netPolicyValueV3` / `netPolicyValueV2` / `dnsAllowValue` 布局静态断言 | ABI 守卫 | 现有 | `netpolicy_test.go` |
| `mvmtap` `classify_egress_flow` 单次 LPM 解析 `(ip,dport)/48`→scheme | 数据面解析 | **新增(go,内核bpf)** | `egress_policy_test.go::TestClassifyEgressFlowLPMFallback` / `TestClassifyEgressFlowExpiredAllow` / `TestClassifyEgressFlowExactL7Match`（提交 `2fdcbc6` / `fc9acac`） |
| `populateAllowOutInnerMap` 落表（`/48` 每端口 + 默认展开） | 用户态落表 | **新增(go)** | `netpolicy_test.go::TestPopulateAllowOutStaticV3OverCoveringLearnedStaysStatic` / `...OverExactLearnedStaticWins` / `...OverExactStaticMergesFlags` / `...PlainStaticOverwritesLearnedUnconditionally` |

## 7. 端到端（`sdk/python/tests`）

| 场景 | 期待 | 覆盖 | 位置 |
|------|------|------|------|
| 自定义 HTTP 端口注入 header（如 18080） | 请求到达目标并带注入头 | 现有 | `test_l7_custom_port_e2e.py::test_l7_custom_http_inject_and_https_deny` |
| 自定义 HTTPS 端口拒绝（如 :1012） | 返回 403 | 现有 | 同上 |
| 自定义 HTTPS 端口放行到真实上游（非 443） | 200 + 响应体 | 现有 | `test_l7_custom_https_allow_reaches_real_upstream` |
| `port` 有 `scheme` 缺 → 提交被拒 | `ValueError` | **新增(e2e)** | `test_l7_custom_port_validation_e2e.py::test_l7_custom_port_requires_scheme` |
| 同 `(host,port)` scheme 冲突 → 整策略被拒 | 提交失败 | **新增(e2e)** | `test_l7_custom_scheme_conflict_rejected` |
| > 8 个 `(port,scheme)` 元组 → 提交被拒 | 提交失败 | **新增(e2e)** | `test_l7_custom_port_budget_exceeded_rejected` |
| 同 host 混合 80/443 + 8080/http + 8443/https | 四元组各生效 | 现网验证 | `test_l7_custom_port_e2e.py` 覆盖自定义端口元组（2026-07-28 实跑通过） |

> e2e 需集群 + `CUBE_E2E=1` / `--run-e2e` + `CUBE_L7_E2E_HTTP_TARGET_HOST` / `CUBE_TEMPLATE_ID`。

---

## 新增文件

| 层 | 文件 | 语言 | 验证 |
|----|------|------|------|
| eBPF 编码 | `CubeNet/cubevs/custom_port_test.go` | Go | `go test` 7 用例全绿，`gofmt` 干净 |
| eBPF 数据面内核回归 | `CubeNet/cubevs/egress_policy_test.go` | Go | `go test`（`TestClassifyEgressFlowLPMFallback` / `TestClassifyEgressFlowExpiredAllow`）全绿，提交 `2fdcbc6` |
| Lua 语义 | `CubeEgress/tests/port_scheme_extra_test.lua` | Lua | `lua tests/port_scheme_extra_test.lua` → PASS（与现有 `port_scheme_test.lua` 同跑通过） |
| Python 校验 | `sdk/python/tests/test_l7_custom_port_validation_e2e.py` | Python | `python3 -m py_compile` 通过，e2e 用例需集群 |
| eBPF DNS 学成数据面 | `CubeNet/cubevs/dns_learn_test.go`（+ `CubeNet/src/dns_learn_test.bpf.c`） | Go | `go test`（`TestDNSLearnPlainAllowWritesIPOnlyEntry` / `TestDNSLearnL7DefaultPortSet` / `TestDNSLearnL7ExplicitPorts` / `TestDNSLearnCoveringStaticCIDRDoesNotImmortalize` / `TestDNSLearnExactStaticEntrySurvivesRefresh` / `TestDNSLearnRefreshRenewsTTLMergesFlagsAndOverwritesScheme`）全绿，提交 `3e725b7`（同键合并用例经变异验证） |
| 迁移 | `CubeNet/cubevs/migration_test.go` | Go | `go test`（`TestSkipUnsupportedL7Subnet`）全绿，提交 `e9e9463` |

> 评审修复轮另在既有测试文件内补充用例：`netpolicy_test.go`（M1 主机/CIDR 归一化合并与冲突、
> M2 subnet 拒绝、M3 条目数展开计数、默认/显式端口展开）、`cube-proxy-iptables-init_test.sh`
> （M5 fwmark 大小写幂等）、`local_service_test.go`（M8 SNI+Host 双投、B1 recover fixture）、
> `test_policy.py`（SDK scheme 小写归一）、`test_l7_custom_port_validation_e2e.py`（M7 断言收紧
> 为 `ApiError, match=`）。

现有覆盖见：`CubeNet/cubevs/netpolicy_test.go`、`CubeEgress/tests/port_scheme_test.lua`、
`CubeEgress/tests/cube-proxy-iptables-init_test.sh`、
`sdk/python/tests/test_l7_custom_port_e2e.py`、
`examples/code-sandbox-quickstart/network_l7_custom_port_echo.py`。

## 缺口小结（建议后续）

1. iptables **任意自定义端口按 mark 选路**的 **iptables 级定向单测**（不依赖 `--dport`）；
   端到端已由 e2e 现网验证（2026-07-28，见文末）。
2. `populateAllowOutInnerMap` 落表已有真实-map 单测覆盖（`netpolicy_test.go`：覆盖学习项不传染
   静态 /48、同键学习遇静态落表静态赢、同键静态 flags 并入、plain 无条件覆盖）；`classify_egress_flow`
   内核 BPF 单测已覆盖（`egress_policy_test.go`，`2fdcbc6`/`fc9acac`）。
3. 同 host **混合 80/443 + 自定义端口** 的四元组 e2e 自动化（自定义端口元组已由
   `test_l7_custom_port_e2e.py` 现网验证，2026-07-28，建议补自动化）。
4. DNS 学成回填的**数据面**已有覆盖（`dns_learn_test.go`：普通 `/32`、L7 默认 `{80,443}`、
   L7 显式端口，`3e725b7`）；但 `dns_query_track` 查询跟踪与 `dns_allow_v2` 在自定义端口下的
   **端到端**（真实 DNS 报文驱动）回填路径仍未自动化。

> 评审修复轮新增并已通过的回归：`buildL7Plan` 主机/CIDR 归一化合并与冲突（M1）、subnet host
> 拒绝（M2）、条目数展开计数（M3）、迁移崩溃安全（M4，stale 备份路径；后经 `9244355` 随 48B
> 格式一并移除）、iptables fwmark 大小写幂等（M5）、迁移 legacy L7 subnet 丢弃
> （`TestSkipUnsupportedL7Subnet`）、SNI+Host 双投（M8）、SDK scheme 归一与 e2e 断言收紧（M7）。
> 遗留环境性问题见设计文档「当前状态」。

---

## 现网验证记录（2026-07-28）

- 环境：one-click dist `7480d03`（构建自 rebase 前同名提交），API `127.0.0.1:3000`。
- `CUBE_E2E=1 CUBE_L7_E2E_HTTP_TARGET_HOST=172.18.0.1 python -m pytest
  tests/test_l7_custom_port_e2e.py -v` → **2/2 通过**：自定义 HTTP 端口注入（本地 echo 目标
  收到注入头）、HTTPS 自定义端口拒绝（`:1012` → 403）、HTTPS 自定义端口放行到真实上游
  （`:1012` → 200，非空 body）。
- 同机验证 **mark 安装期可配置**：覆盖 `CUBE_L7_MARK_HTTP=0xA0000000` /
  `CUBE_L7_MARK_HTTPS=0xB0000000`（mask `0xF0000000`）后，eBPF 印记与 iptables/ip rule
  同步切换，TPROXY 计数器持续命中——`/etc/cubeegress/l7-marks.conf` 单一来源、双消费方
  一致性成立。
- 同机运行示例 `network_l7_custom_port_echo.py` → **6/6 探针通过**：scheme-only 默认 :80/:443、
  裸 host 规则（无 port/scheme）同规则覆盖默认集 :80+:443、自定义 `:18080`/http（本地 echo
  收到注入 marker）、自定义 `:1012`/https（200 + 非空 body）。
- 运维提示：用 `example.com` 演示自定义端口会超时（其 8080/8443 在握手后被静默丢弃，宿主机
  直连同现象），并非 CubeEgress 故障；演示成功路径请用可响应的目标（如 e2e 的本地 echo）。
