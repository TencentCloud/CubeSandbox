# Google ADK + CubeSandbox 集成示例

这个示例把 CubeSandbox 暴露成一个 Google ADK 函数工具。ADK agent 仍在你的本地开发环境运行，但它生成的 Python 代码会通过 E2B-compatible SDK 进入临时 CubeSandbox MicroVM 中执行。

## 文件

```text
google-adk-integration/
  agent.py            ADK root_agent 定义
  cube_code_tool.py   基于 CubeSandbox 的 Python 执行工具
  smoke_test.py       离线导入和接线检查
  .env.example        必需环境变量
  requirements.txt
```

## 配置

```bash
cd examples/google-adk-integration
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

编辑 `.env`，填入 CubeAPI 地址、CubeSandbox 模板 ID 和 Google API key。本地未启用认证的 CubeAPI 可使用 `E2B_API_KEY=e2b_000000`。

## 运行检查

```bash
python smoke_test.py
```

期望输出：

```text
GOOGLE_ADK_CUBE_SMOKE_OK
```

## 运行 ADK agent

从包含该示例目录的父目录运行：

```bash
adk run google-adk-integration
```

向 agent 提问：

```text
Run Python in the sandbox to calculate the first 10 Fibonacci numbers.
```

agent 应调用 `run_python_in_cube`，基于 `CUBE_TEMPLATE_ID` 创建 CubeSandbox，执行 Python 代码，返回 stdout，并在工具调用结束后删除临时沙箱。

## 注意事项

- 请使用支持 E2B code interpreter `run_code` 路径的模板。
- E2B 相关包已固定到一个 plain `pip` 可直接安装、且记录在仓库 SDK compatibility notes 中的组合。修改任一版本前请重新验证。
- `CUBE_SANDBOX_TIMEOUT` 控制临时沙箱生命周期，`CUBE_RUN_CODE_TIMEOUT` 控制每次 `run_code` 执行。
- 本地开发没有泛域名 DNS 时，可以设置 `CUBE_USE_DEV_SIDECAR=true`，并在本示例的 `.env`
  中配置 `CUBE_REMOTE_PROXY_*` 变量，或把它们导出到 ADK 进程环境中。不要只修改
  `examples/e2b-dev-sidecar/.env`；本示例不会加载那个文件。
- 如果 CubeAPI 使用自签证书，请设置 `CUBE_SSL_CERT_FILE`。这会在进程级导出 `SSL_CERT_FILE`，
  因此 CA bundle 也需要保留模型提供商的 TLS 信任。
