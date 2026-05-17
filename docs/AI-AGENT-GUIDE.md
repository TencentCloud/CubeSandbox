# AI Agent 开发指南

本文档为 AI Agent（如 Hermes、Claude Code、Codex 等）在 CubeSandbox 项目中的开发提供指导。

## 项目概述

CubeSandbox 是一个轻量级沙箱环境，用于安全地运行和管理容器化应用。项目采用 Rust 编写，支持多种平台。

## 代码风格

### Rust 代码规范

- 使用 `cargo fmt` 格式化代码
- 使用 `cargo clippy` 检查代码质量
- 遵循 Rust API 指南（https://rust-lang.github.io/api-guidelines/）
- 错误处理使用 `thiserror` 或 `anyhow` crate
- 日志使用 `tracing` crate

### 提交规范

- 提交信息使用 Conventional Commits 格式
- 示例：`feat: add new container runtime support` 或 `fix: resolve memory leak in sandbox`

## 项目结构

```
CubeSandbox/
├── hypervisor/     # 核心沙箱实现
├── net-agent/      # 网络代理
├── tpl-agent/      # 模板代理
├── docs/           # 文档
└── scripts/        # 构建和部署脚本
```

## 常见任务

### 构建项目

```bash
cargo build --release
```

### 运行测试

```bash
cargo test
cargo clippy
```

### 检查代码

```bash
cargo fmt --check
cargo clippy --all-targets --all-features
```

## AI Agent 注意事项

1. **代码审查**：AI 生成的代码需要人类审查后才能合并
2. **DCO 签名**：AI 不能添加 Signed-off-by 标签，只有人类可以
3. **贡献标签**：在提交信息中添加 `Assisted-by:` 或 `Autonomously-by:` 标签
4. **测试覆盖**：新增功能需要添加相应测试
5. **文档更新**：修改功能时同步更新相关文档

## 参考资源

- [Rust 官方文档](https://doc.rust-lang.org/)
- [CubeSandbox Wiki](https://github.com/TencentCloud/CubeSandbox/wiki)
- [贡献指南](CONTRIBUTING.md)
