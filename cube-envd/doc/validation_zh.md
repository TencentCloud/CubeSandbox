# cube-envd 功能验收报告

[English](validation.md) · [组件概览](../README_zh.md) · [构建与接入](usage_zh.md)

本报告整理已完成的本地功能验证，说明实际结果、验证边界及复跑
入口。整理报告期间没有重新执行这些测试。[脱敏日志摘录](validation/functional-excerpts.md)
随文档提供，包含原始测试名称、结果、daemon 身份和失败后的复验记录。

## 结论与交付范围

Rust cube-envd 已在本地 CubeSandbox 平台完成基础镜像接入、新模板创建与 READY、
SDK 创建 sandbox、daemon 身份与健康检查、命令执行和文件读写验证。Go 对照使用
同一组功能断言，也通过完整链路及三个独立场景。

**Go 保持默认，Rust 由用户自行构建并显式选择。** 这是本次交付范围；并未实施
原需求中的默认 Rust 切换。构建 Rust 使用仓库自身源码，不需要编译上游 Go envd；
默认 Go 构建入口仍保留上游依赖。该范围调整应在 PR 中明确说明。

本报告只评估功能，不包含性能对比，也不作性能优势或性能达标承诺。

## 验证环境与源码身份

| 项目 | 实际受测配置 |
| --- | --- |
| 平台 | 本地已部署 CubeSandbox，cube-runtime v0.5.0，Cloud Hypervisor |
| 平台架构 | 原生 x86_64 / amd64 |
| guest | Ubuntu 22.04.5 LTS；kernel `6.6.69-opencloudos9.cubesandbox.pvm.guest-gb85200d80fa2`；cgroup v1 |
| sandbox 资源 | 1 vCPU、512 MiB，以 sandbox info 为准 |
| SDK / runner | 仓库 CubeSandbox Python SDK 0.7.0；Python 3.10.12 |
| Rust 工具链 | Rust/Cargo 1.89.0，配套 rustfmt 和 Clippy |
| Rust daemon | 产品版本 0.1.0；envd 兼容版本 0.5.7 |
| Go 对照 | `e2b-dev/infra@2026.16`，源码 `b8ca332f435370397bf42be614b2a5b620d65d39`；程序报告版本 0.5.13 |
| arm64 镜像运行 | 全系统 QEMU TCG，2 vCPU、1536 MiB；不是原生 arm64 平台验收 |

SDK 请求经过 CubeProxy，没有直接访问 guest IP 绕过代理。镜像在本地构建和使用，
没有发布。最终独立场景使用的 Go 派生镜像补齐 socat/libwrap0，以对齐软件包；
这没有改变默认 Go Dockerfile。

受测 Rust 二进制的 `-commit` 为 `2a377149915e550a7f7705824739737de3458898`：
这是镜像构建时的 HEAD，构建同时包含随后提交的未提交源码修改，不能据此将受测
二进制描述为该基点的干净构建。实现最终提交为 `0e8190ca68f2de19bb277c60fdd4ff3dfcb552cc`，
对应 Git tree `c73154f791c4595a9eada655e67c752c58c90b74`。本报告不声称在该提交后
重建并重跑了全部测试，后续文档与 CI 配置整理也不属于这次运行证据。

## 真实平台功能验收

| 场景 | 断言范围 | Rust | Go |
| --- | --- | --- | --- |
| 新模板完整链路 | 从镜像创建模板、观察 READY、创建 sandbox、确认运行中程序身份、health 204、命令、文件与清理 | 通过 | 通过 |
| 独立健康与身份 | 检查实际运行的 `/usr/bin/envd`、源码 revision 及经代理的 health 204 | 通过 | 通过 |
| 独立命令执行 | stdout/stderr、非零退出、环境变量、默认 root、显式 UID 1000、超时 | 通过 | 通过 |
| 独立文件读写 | UTF-8、空文件、覆盖截断、文件不存在、SDK 与命令双向读写、二进制写入后经命令核对 | 通过 | 通过 |

以上包括 **两条完整链路和六个独立场景运行**；表内的各条断言并非各自独立的平台
测试次数。二进制校验不代表 SDK 已支持二进制 read。模板创建、sandbox 操作和
清理使用已有 SDK E2E 框架；完整链路日志包含 cleanup 事件，errors 为空。

测试入口：[test_public.py](../../tests/e2e/sdk_compat/cases/envd/test_public.py)。
实际断言：[envd_acceptance.py](../../tests/e2e/sdk_compat/framework/envd_acceptance.py)。

## 组件、框架与镜像验证

| 层次 | 实际结果 | 验证边界 |
| --- | --- | --- |
| Rust 组件 CI，amd64 | fmt、Clippy、build、check 完成；184 项 Rust 测试通过，3 项 ignored | ignored 是需要 VM 的长运行夹具，不计通过 |
| CI 内嵌 Python 场景 | 6 项通过 | 已被 Rust 测试入口调用，不与 Rust 数量相加形成独立总数 |
| CI 内 guest 服务场景 | 9 项通过，1 项 IPv6 skip | skip 不计通过；属于同次组件 CI |
| SDK E2E 框架自身测试 | 122 项通过 | 是框架测试，不代表 122 项真实平台场景 |
| amd64 镜像，Rust / Go | 分别 18/18 通过 | 包含镜像与共享 supervisor 测试 |
| arm64 镜像，Rust | QEMU 下 18/18 通过 | 不代表原生 arm64 平台通过 |
| arm64 镜像，Go | 原轮 7 项 image 测试通过；11 项 supervisor 中 1 项失败 | 原轮不能标为全部通过 |
| supervisor 夹具修复后复验 | amd64 11/11；QEMU arm64 11/11 | 独立重跑共享 supervisor；不是 Go arm64 整轮重跑 |

组件测试还覆盖 Process/PTY 生命周期、输入输出和信号、Filesystem 元数据与目录
监听、HTTP 文件传输、认证、协议编解码及错误行为。具体用例名称见日志摘录；
这些组件证据与经 CubeProxy 的真实平台证据分开解读。

Go arm64 原轮失败来自测试 worker 读取尚未写完的退出码文件，触发
`ValueError: invalid literal for int() with base 10: ''`。夹具改为原子发布后，共享
supervisor 两种架构复验通过。附件同时保留失败与复验结果。

## 兼容范围与限制

- 兼容范围是当前已实现的 HTTP 接口与 Process/Filesystem 协议，以及本报告所列
  SDK 调用；不同的 Go/Rust 版本字符串不意味着所有上游版本和功能均已验证。
- guest 使用 cgroup v1 时，Rust 的 cgroup v2 初始化记录警告并使用 no-op manager，
  与 Go 的初始化降级方式一致。子进程继承已有 cgroup；不再施加 envd 额外分组。
  成功启用 v2 后的进程归组错误仍拒绝对应进程，平台和 VM 的既有隔离不因此取消。
- 未完成原生 arm64 组件及平台验收、Firecracker 平台验收和远端 GitHub Actions
  执行。三项 ignored VM 夹具与 IPv6 skip 不属于已验证能力。
- 本地 CI 采用源码只读挂载、私有 cgroup 与移除 SYS_TIME 的测试环境；报告中的
  成功不依赖修改宿主 cgroup 或时钟。复跑应沿用仓库中的隔离配置。

## 自行构建与复跑

1. 按[组件构建说明](../README_zh.md#构建组件)编译，使用
   `make cube-base-rust` 显式选择 Rust 镜像。
2. 按[模板与 SDK 指南](usage_zh.md)配置自己的平台地址和凭据，并使用构建节点
   能读取的镜像创建模板。
3. 按[选定 envd 验收](../../tests/e2e/sdk_compat/README_zh.md#选定-envd-验收)
   运行完整链路或三个独立场景；保存该次生成的 `events.jsonl` 和测试输出。
4. 组件与镜像测试的入口见 [README 测试节](../README_zh.md#测试)。运行前核对
   当前工具链、架构及隔离要求，按实际结果记录失败、跳过和未运行项。

本报告的日志是历史运行证据，不是构建输入。读者无需具有作者的目录结构、镜像
缓存或原平台资源 ID，也不应把自己的新运行结果归入本次报告。
