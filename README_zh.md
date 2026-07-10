<p align="center">
  <img src="docs/assets/cube-sandbox-logo.png" alt="Cube Sandbox Logo" width="140" />
</p>

<h1 align="center">CubeSandbox</h1>

<p align="center">
  <strong>一个极速启动、高并发、安全且轻量化的 AI Agent 沙箱服务</strong>
</p>


<p align="center">
  <a href="https://trendshift.io/repositories/26338?utm_source=repository-badge&amp;utm_medium=badge&amp;utm_campaign=badge-repository-26338" target="_blank" rel="noopener noreferrer"><img src="https://trendshift.io/api/badge/repositories/26338" alt="TencentCloud/CubeSandbox | Trendshift" width="250" height="55" /></a>
</p>

<p align="center">
  <a href="https://github.com/tencentcloud/CubeSandbox/stargazers"><img src="https://img.shields.io/github/stars/tencentcloud/cubesandbox?style=social" alt="GitHub Stars" /></a>
  <a href="https://github.com/tencentcloud/CubeSandbox/issues"><img src="https://img.shields.io/github/issues/tencentcloud/cubesandbox" alt="GitHub Issues" /></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0 License" /></a>
  <a href="./CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen" alt="PRs Welcome" /></a>
  <a href="https://landscape.cncf.io/?landscape=observability-and-analysis&group=ai-native&item=ai-native-infra--workload-runtime--cubesandbox"><img src="https://img.shields.io/badge/CNCF-Landscape-0C66E4" alt="CNCF Landscape" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/⚡_启动-极速启动-blue" alt="极速启动" />
  <img src="https://img.shields.io/badge/🔒_隔离-硬件级强隔离-critical" alt="硬件级强隔离" />
  <img src="https://img.shields.io/badge/🔌_接口-兼容_E2B-blueviolet" alt="E2B 兼容" />
  <img src="https://img.shields.io/badge/📦_部署-高并发·高密度-orange" alt="高并发·高密度" />
</p>


<p align="center">
  <a href="./README.md"><strong>English</strong></a> ·
  <a href="./docs/zh/guide/quickstart.md"><strong>快速开始</strong></a> ·
  <a href="./docs/zh/index.md"><strong>文档</strong></a> ·
  <a href="./docs/zh/changelog/index.md"><strong>变更日志</strong></a> ·
  <a href="#wechat-group"><strong>微信交流群</strong></a> ·
  <a href="https://x.com/CubeSandbox_AI"><strong>X(Twitter)</strong></a> ·
  <a href="https://wj.qq.com/s2/26753159/ss16/"><strong>核心用户计划</strong></a>
</p>

---

Cube Sandbox 是一款基于 RustVMM 与 KVM 构建的高性能、开箱即用的安全沙箱服务。它既支持单机部署，也能方便地扩展到多机集群。对外兼容 E2B SDK，可在 60ms 内创建具备完整服务能力的硬件隔离沙箱，并将内存开销控制在 5MB 以内。


<p align="center">
  <img src="./docs/assets/readme_speed_zh_1.png" width="400" />
  <img src="./docs/assets/readme_overhead_zh_1.png" width="400" />
</p>

## 📰 动态

<table>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/zh/changelog/v0.5.0.md">
        <img src="https://img.shields.io/badge/v0.5.0-2026.07.03-dc3545?style=flat-square" alt="v0.5.0" />
      </a>
    </td>
    <td valign="top">
      <strong>v0.5：自动暂停恢复、Terraform 一键集群、ARM64 全栈支持、沙箱网络策略增强</strong><br/>
      <b>自动暂停恢复</b> — 闲置沙箱自动暂停，新请求到达时毫秒级唤醒。<b>Terraform 一键集群部署</b> <b>ARM64</b> 全栈原生支持 <b>沙箱网络策略增强</b> — 流量令牌鉴权、出向策略路由。<br/>
      <a href="./docs/zh/changelog/v0.5.0.md">更新日志 →</a> ·
      <a href="./docs/zh/guide/tencentcloud-terraform-deploy.md">Terraform 部署 →</a>
    </td>
  </tr>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/zh/changelog/v0.4.0.md">
        <img src="https://img.shields.io/badge/v0.4.0-2026.06.14-6f42c1?style=flat-square" alt="v0.4.0" />
      </a>
    </td>
    <td valign="top">
      <strong>v0.4：出站更安全，运维更省心</strong><br/>
      <b>凭证托管</b> — Agent 照常调外部 API，Key 不进沙箱。<b>控制台</b> — 版本矩阵、模板健康检查，升级后该不该重建一眼可见。<br/>
      <a href="./docs/zh/changelog/v0.4.0.md">更新日志 →</a> ·
      <a href="./docs/zh/guide/security-proxy.md">安全代理指南 →</a> ·
      <a href="./docs/zh/guide/webui.md">WebUI 指南 →</a>
    </td>
  </tr>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/zh/changelog/v0.3.0.md">
        <img src="https://img.shields.io/badge/v0.3.0-2026.06.02-007bff?style=flat-square" alt="v0.3.0" />
      </a>
    </td>
    <td valign="top">
      <strong>百毫秒级快照、克隆与回滚能力</strong><br/>
      CubeSandbox 0.3.0 引入 <b>CubeCoW</b> Copy-on-Write 快照引擎，支持沙箱状态的事件级快照、即时克隆以及回滚到任意历史状态。
      <a href="./docs/zh/changelog/v0.3.0.md">更新日志 →</a>
    </td>
  </tr>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/zh/changelog/v0.1.0.md">
        <img src="https://img.shields.io/badge/v0.1.0-2026.04.20-28a745?style=flat-square" alt="v0.1.0" />
      </a>
    </td>
    <td valign="top">
      <strong>🎉 正式开源首发</strong><br/>
      Cube Sandbox 正式开源！毫秒级启动、硬件级隔离、E2B 兼容的 AI Agent 安全沙箱服务。
      <a href="./docs/zh/changelog/v0.1.0.md">更新日志 →</a>
    </td>
  </tr>
</table>

## 产品能力一览

<table align="center">
  <tr align="center" valign="top">
    <td width="33%">
      <strong>⚡ 毫秒启动 · 高密度· 自动暂停恢复</strong><br/><br/>
      平均 &lt;60ms 冷启动，单实例额外开销 &lt;5MB，单机轻松跑起数千 Agent。支持沙箱的自动暂停及恢复，实现成本优化<br/><br/>
      <a href="./docs/zh/guide/quickstart.md">快速开始 →</a>
    </td>
    <td width="33%">
      <strong>🔒 硬件级隔离</strong><br/><br/>
      每个沙箱独立 Guest OS 内核，告别 Docker 共享内核，放心跑大模型生成的未知代码<br/><br/>
      <a href="./docs/zh/architecture/overview.md">架构概览 →</a>
    </td>
    <td width="33%">
      <strong>🔌 E2B 无缝迁移</strong><br/><br/>
      原生兼容 E2B SDK，替换一个 URL 环境变量即可接入，零业务代码改动<br/><br/>
      <a href="./docs/zh/guide/tutorials/examples.md">示例项目 →</a>
    </td>
  </tr>
  <tr align="center" valign="top">
    <td width="33%">
      <strong>🖥️ Web 控制台</strong><br/><br/>
      浏览器管集群：沙箱、模板、节点、版本矩阵，装完即开 <code>:12088</code><br/><br/>
      <a href="./docs/zh/guide/webui.md">WebUI 指南 →</a>
    </td>
    <td width="33%">
      <strong>🔐 凭证托管</strong><br/><br/>
      Agent 照常调 LLM 与外部 API，Key 不进沙箱、不进模型上下文、不落日志<br/><br/>
      <a href="./docs/zh/guide/security-proxy.md">安全代理指南 →</a>
    </td>
    <td width="33%">
      <strong>🛡️ 出站管控</strong><br/><br/>
      域名白名单放行、越权出站当场拦截，全量访问留审计日志方便合规<br/><br/>
      <a href="./docs/zh/guide/security-proxy.md">安全代理指南 →</a>
    </td>
  </tr>
  <tr align="center" valign="top">
    <td width="33%">
      <strong>📸 快照 · 克隆 · 回档</strong><br/><br/>
      百毫秒级检查点，运行中随时快照，回滚到任意状态或分叉探索<br/><br/>
      <a href="./docs/zh/changelog/v0.3.0.md">v0.3 更新日志 →</a>
    </td>
    <td width="33%">
      <strong>📦 模板体系</strong><br/><br/>
      OCI 镜像一键转模板，模板商店装官方预置环境，跨节点自动分发<br/><br/>
      <a href="./docs/zh/guide/templates.md">模板指南 →</a>
    </td>
    <td width="33%">
      <strong>🤖 AgentHub 数字助手</strong><br/><br/>
      基于 OpenClaw 一键创建 AI 助手，支持快照、回档与助手模板发布<br/><br/>
      <a href="./docs/zh/guide/digital-assistant.md">数字助手 →</a>
    </td>
  </tr>
</table>

## 视频演示

<table align="center">
  <tr align="center" valign="middle">
    <td width="25%" valign="middle">
      <video src="https://github.com/user-attachments/assets/f87c409e-29fc-4e86-9eac-dbeaff2aca18" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
    <td width="25%" valign="middle">
      <video src="https://github.com/user-attachments/assets/50e7126e-bb73-4abc-aa85-677fdf2e8c67" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
    <td width="25%" valign="middle">
      <video src="https://github.com/user-attachments/assets/052e0e77-e2d9-409e-90b8-d13c28b80495" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
    <td width="25%" valign="middle">
      <video src="https://github.com/user-attachments/assets/c8845a84-5792-4062-ae9d-4787c24f5a58" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
  </tr>
  <tr align="center" valign="top">
    <td>
      <em>安装及功能演示</em>
    </td>
    <td>
      <em>性能测试</em>
    </td>
    <td>
      <em>RL 场景 (SWE-Bench)</em>
    </td>
    <td>
      <em>快照 · 克隆 · 回档</em>
    </td>
  </tr>
</table>


## 性能与方案对比 (Benchmarks)

在 AI Agent 代码执行场景下，Cube Sandbox 实现了安全与性能的兼得：

| 维度 | Docker 容器 | 传统虚拟机 (VM) | CubeSandbox |
|---|---|---|---|
| **隔离级别** | 低 (共享内核 Namespaces) | 高 (独立内核) | **极高 (独立内核 + eBPF网络隔离)** |
| **启动速度** <br>*完整启动OS时长 | 200ms | 秒级 | **毫秒级 (< 60ms)** |
| **内存开销** | 低（共享内核） | 高 (完整 OS ) | **低 (极限裁剪，< 5MB)** |
| **部署密度** | 高 | 低 | **极高 (单机数千实例)** |
| **E2B SDK 兼容** | / | / | **✅ 完全兼容 (Drop-in)** |

> *Cube Sandbox 测试数据说明：其中，启动速度项基于裸金属环境测试，单并发下为 60ms，50 并发场景下平均 67ms（P95 90ms，P99 137ms），整体保持在百毫秒级。内存开销项基于 ≤ 32GB 规格沙箱实测，更大规格下开销会略有上升，但幅度极小。*

详细的创建时延和资源消耗情况可参考 [核心操作性能基准测试报告（裸金属）](./docs/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md) 与 [PVM 云服务器测试报告](./docs/zh/blog/posts/2026-06-03-cubesandbox-perf-benchmark-pvm.md)。

<table align="center">
  <tr align="center" valign="middle">
    <td width="33%" valign="middle">
      <img src="./docs/assets/1-concurrency-create.png" />
    </td>
    <td width="33%" valign="middle">
      <img src="./docs/assets/50-concurrency-create.png" />
    </td>
    <td width="33%" valign="middle">
      <img src="./docs/assets/cube-sandbox-mem-overhead.png" />
    </td>
  </tr>
  <tr align="center" valign="top">
    <td colspan="2">
      <em>单 / 高并发场景下百毫秒级的沙箱交付</em>
    </td>
    <td>
      <em>不同规格沙箱 Cube Sandbox 自身内存消耗</em><br>
      <sup>*其中蓝色部分为沙箱规格，橙色部分为对应规格下消耗内存，随着规格扩大，内存消耗呈现少量增长</sup>
    </td>
  </tr>
</table>

## 快速开始

</br>

<p align="center">
  <img src="docs/assets/fast-start.gif" alt="Cube Sandbox 毫秒级启动演示" width="720" />
</p>

<p align="center">
  <em>⚡ 毫秒级启动 —— 观看上方快速启动流程演示。</em>
</p>

Cube Sandbox 需要一台支持 **KVM** 的 **x86_64 Linux** 环境。

指南带你**四步**完成全部流程 —— 准备服务器、安装 Cube Sandbox、创建沙箱模板、运行第一段 Agent 代码。无需编译源码，几分钟即可上手。

<p align="center">
  <b>选择你的部署方式：</b>
</p>

<table align="center">
  <tr align="center">
    <td align="center">
      <a href="./docs/zh/guide/pvm-deploy.md" style="
        display: inline-block;
        background: #28a745;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        🖥 PVM · 云服务器部署 →
      </a>
      <br/>
      <sup><b>🏆 推荐</b></sup>
    </td>
    <td align="center">
      <a href="./docs/zh/guide/bare-metal-deploy.md" style="
        display: inline-block;
        background: #007bff;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        🏗 裸金属部署 →
      </a>
    </td>
    <td align="center">
      <a href="./docs/zh/guide/dev-environment.md" style="
        display: inline-block;
        background: #6c757d;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        💻 Dev-Env →
      </a>
      <br/>
      <sup>⚠️ <b>不推荐 — 性能差</b></sup>
    </td>
  </tr>
</table>

### 装完第一件事：打开 Web 控制台

<p align="center">
  <img src="docs/assets/webui-demo.gif" alt="WebUI 控制台演示" width="720" />
</p>

<p align="center">
  <em>🖥️ 可视化管理 —— 从概览到创建沙箱、查看日志，全程在浏览器完成。</em>
</p>

一键部署完成后，在浏览器访问：

```
http://<控制节点 IP>:12088
```


**推荐三步：**

1. **先看概览** — 打开 **Overview**，确认节点 Ready、资源有余量，集群可以正常接单
2. **备好模板** — 到 **Template Store** 一键安装官方预置镜像；若 **Templates** 里已有 `READY` 模板可跳过
3. **创建沙箱** — **Sandboxes → + New sandbox**，选 `READY` 模板创建，几秒内进入详情页查看实时日志


完整说明见 [WebUI 控制台指南](./docs/zh/guide/webui.md)。

## 深入探索

- [文档首页](./docs/zh/index.md) — 完整指南导航
- ☁️ [PVM 部署](./docs/zh/guide/pvm-deploy.md) — 在普通云服务器上部署，无需裸金属或嵌套虚拟化
- [模板概览](./docs/zh/guide/templates.md) — 镜像到模板的概念与工作流
- [示例项目](./docs/zh/guide/tutorials/examples.md) — 展示各种使用场景的示例（涵盖代码执行、浏览器自动化、OpenClaw 集成与 RL 训练等）
- 🖥️ [WebUI 控制台](./docs/zh/guide/webui.md) — 装完即用的可视化管理（`:12088`）
- 🔐 [安全代理与凭证托管](./docs/zh/guide/security-proxy.md) — CubeEgress 域名过滤、注入与审计
- 🤖 [数字助手 AgentHub](./docs/zh/guide/digital-assistant.md) — OpenClaw 助手创建与管理（Preview）
- 💻 [开发环境（QEMU 虚机）](./docs/zh/guide/dev-environment.md) — 暂时没有 KVM 访问权限？在一次性的 OpenCloudOS 9 虚机里体验 Cube Sandbox

## 架构概览

<p align="center">
  <img src="docs/assets/cube-sandbox-arch.png" alt="Cube Sandbox 架构图" />
</p>

| 组件 | 职责 |
|---|---|
| **CubeAPI** | 兼容 E2B 的 REST API 网关（Rust），替换 URL 即可从 E2B 无缝切换。 |
| **CubeMaster** | 编排调度器，接收 API 请求并分发到对应 Cubelet，负责资源调度与集群状态维护。 |
| **CubeProxy** | 反向代理，兼容 E2B 协议，将请求路由到对应沙箱。 |
| **Cubelet** | 计算节点本地调度组件，管理单节点所有沙箱实例的完整生命周期。 |
| **CubeVS** | 基于 eBPF 内核态转发的虚拟交换机，提供网络隔离与安全策略支持。 |
| **CubeEgress** | 基于 OpenResty 的出站安全网关：L7 域名过滤、凭证注入、访问审计；与 CubeVS 内核策略配合，沙箱流量不可绕过。 |
| **CubeHypervisor & CubeShim** | 虚拟化层 —— CubeHypervisor 负责管理 KVM MicroVM，CubeShim 实现 containerd Shim v2 接口，将沙箱集成到容器运行时。 |

详见[架构概览](./docs/zh/architecture/overview.md)和 [CubeVS 网络模型](./docs/zh/architecture/network.md)。

## 社区与贡献

我们欢迎各种形式的贡献——Bug 报告、功能建议、文档改进、代码提交。

- **发现 Bug** —— <a href="https://github.com/tencentcloud/CubeSandbox/issues" target="_blank">在这里报告问题或提出建议</a>
- **有新想法** —— <a href="https://github.com/tencentcloud/CubeSandbox/discussions" target="_blank">提问交流与想法分享</a>
- **想写代码？** —— 查看我们的 <a href="./CONTRIBUTING.md" target="_blank">CONTRIBUTING.md</a> 贡献指南，了解如何提交 Pull Request。
- **想贡献文档 / PR？** —— 欢迎按双语方式投稿到这 3 个社区文档入口：<a href="./docs/zh/guide/troubleshooting/index.md" target="_blank">故障排障</a>、<a href="./docs/zh/guide/usecases/index.md" target="_blank">应用案例</a>、<a href="./docs/zh/guide/integrations/index.md" target="_blank">生态集成</a>。
- **想成为最终用户？** —— 点击<a href="https://wj.qq.com/s2/26499618/a9fc/" target="_blank">这里</a>填写用户调研。
- **想聊聊天？** —— 扫描二维码，加入我们的微信交流群。


---

<a id="wechat-group"></a>
<p align="center">
  <img src="./docs/assets/wechat_group.jpg" width="220" />
</p>
<p align="center">
  <em>💬 扫描上方二维码加入微信交流群，与核心开发者和社区伙伴零距离沟通！</em>
</p>


## Roadmap

**即将上线** — 详见[完整路线图](./docs/zh/guide/roadmap.md)。

| 特性 | 说明 |
|---|---|
| **Kubernetes 原生部署** | 使用 CRD、Operator 和原生调度在 K8s 集群内完整部署运营 CubeSandbox，无需额外编排组件 |
| **Volume 支持** | 兼容 E2B Volume 协议的持久化与共享存储能力 |
| **跨机暂停与恢复** | 在一台宿主机上暂停沙箱，在另一台上完整恢复内存和文件系统状态 |
| **E2B API 对齐补齐** | 补齐与 E2B 规范的剩余差距，实现完整的兼容替代 |
| **控制面与数据面分离** | 解耦控制面与数据面，控制面升级或故障不影响已在运行的沙箱，保证全路径高可用 |
| **沙箱异常恢复** | 自动检测并恢复 VM Crash、shim 卡死、网络分区等异常，支持可配置恢复策略 |
| **调度与运维能力增强** | 资源感知调度、亲和规则、在线均衡与带迁移的节点排空 |

## 许可证

Cube Sandbox 使用 [Apache License 2.0](./LICENSE) 开源许可证。

Cube Sandbox 的诞生离不开开源社区的基石，特别鸣谢 [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor)、[Kata Containers](https://github.com/kata-containers/kata-containers)、virtiofsd、containerd-shim-rs、ttrpc-rust 等。部分组件为适配 Cube Sandbox 运行模型进行了定制修改，原始上游归属声明均已保留。

---

<p align="center">
  <a href="https://landscape.cncf.io/?landscape=observability-and-analysis&group=ai-native&item=ai-native-infra--workload-runtime--cubesandbox">
    <img src="https://raw.githubusercontent.com/cncf/artwork/refs/heads/main/other/cncf-landscape/horizontal/color/cncf-landscape-horizontal-color.svg" width="300" alt="CNCF Landscape" />
  </a>
</p>
<p align="center">
  Cube Sandbox 已被收录至 <a href="https://landscape.cncf.io/?landscape=observability-and-analysis&group=ai-native&item=ai-native-infra--workload-runtime--cubesandbox">CNCF Landscape</a>。
</p>
