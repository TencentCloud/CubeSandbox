# `sandbox-code` 派生镜像：让 `run_code` 继承沙箱级环境变量

[English](README.md)

本示例提供一个基于官方 `sandbox-code` 的派生镜像，仅替换其中的 lightweight code interpreter，让 `run_code` 读取通过 `Sandbox.create(envs=...)` 注入的沙箱环境变量，并保留单次调用环境变量的临时覆盖语义。

## 背景

创建沙箱时，可以通过 `Sandbox.create(envs=...)` 注入环境变量。这些变量可以被 `commands.run` 读取，但官方 `sandbox-code` 镜像中的 `run_code` 默认无法读取，因为其 lightweight code interpreter 不会将 envd 中的沙箱环境变量注入 Jupyter kernel。

本派生镜像补齐该行为，使 `run_code` 和 `commands.run` 继承相同的沙箱环境变量，同时不修改默认 `sandbox-code` 镜像或已有模板。

## 环境变量语义

每个 Jupyter kernel 第一次执行用户代码前，lightweight code interpreter 会从 `http://127.0.0.1:49983/envs` 读取并缓存当前沙箱的环境变量，然后将其注入 kernel。每次 `run_code` 调用再叠加本次传入的 `env` 或 `env_vars`，优先级如下：

```text
Sandbox.create(envs=...) < run_code 单次调用环境变量
```

沙箱级值和单次调用覆盖会在用户代码执行前，通过单独的后台 kernel execution 注入，因此首个用户 cell 编译失败不会阻止环境初始化。如果两层环境中存在同名变量，本次执行使用单次调用传入的值。在应用单次调用值前，解释器会记录每个受影响变量在 kernel 中的原有状态。执行结束后，后台清理任务会恢复环境：

- 调用前已存在的变量会恢复为原有 kernel 值。
- 调用前不存在的变量会被删除。

下一次执行会先等待上一轮清理完成，避免单次调用环境变量泄漏到后续执行中。该生命周期与 E2B code-interpreter 保持一致。沙箱环境变量在每个 Jupyter kernel 中只读取一次，本示例不提供通用的运行时刷新机制。

本派生解释器依赖 envd 的 `/envs` 接口。如果 envd 不可用、接口不受支持、请求超过 `ENVD_TIMEOUT`（默认 2 秒），或返回的数据无效，`run_code` 会返回 HTTP 502，不会回退到官方解释器的原有行为。可以通过镜像环境变量 `ENVD_TIMEOUT` 调整读取超时时间。

## 构建并测试镜像

在仓库根目录执行：

```bash
docker build \
  -t sandbox-code-env-inheritance:latest \
  examples/run-code-env-inheritance
```

如果需要使用国际站镜像仓库，或指定固定版本的基础镜像，可以覆盖 `SANDBOX_CODE_IMAGE`：

```bash
docker build \
  --build-arg SANDBOX_CODE_IMAGE=cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  -t sandbox-code-env-inheritance:latest \
  examples/run-code-env-inheritance
```

构建完成后，可以直接在镜像内运行聚焦单元测试，以复用实际运行时中的 Python 依赖：

```bash
docker run --rm \
  --entrypoint python \
  -v "$PWD/examples/run-code-env-inheritance:/work:ro" \
  -e PYTHONPATH=/work/lightweight-code-interpreter \
  sandbox-code-env-inheritance:latest \
  -m unittest discover -s /work/tests -v
```

## 创建 CubeSandbox 模板

将镜像推送到 CubeMaster 可以访问的镜像仓库，然后创建模板：

```bash
cubemastercli tpl create-from-image \
  --image <registry>/sandbox-code-env-inheritance:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49999 \
  --probe-path /health
```

创建命令会返回 `job_id`。使用该 ID 等待异步构建完成：

```bash
cubemastercli tpl watch --job-id <job_id>
```

### one-click 部署中使用本地镜像

如果 CubeMaster 无法访问镜像仓库，可以使用 one-click 控制面节点上的本地 Docker 镜像。先在该节点构建或加载 `sandbox-code-env-inheritance:latest`，再将以下配置加入 `/usr/local/services/cubetoolbox/.one-click.env`：

```bash
CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED=false
```

重启 CubeMaster，使配置生效：

```bash
systemctl restart cube-sandbox-cubemaster.service
```

该配置会关闭默认的 native rootfs exporter，并回退到能够读取本地 Docker 镜像的 Docker-based exporter。之后可以直接使用本地镜像标签创建模板：

```bash
cubemastercli tpl create-from-image \
  --image sandbox-code-env-inheritance:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49999 \
  --probe-path /health
```

## 运行 SDK 兼容性 E2E

创建模板后，可以运行 CubeSandbox/E2B 共用的 SDK 兼容性用例，验证沙箱环境变量与单次调用环境变量的合并、覆盖和清理行为：

```bash
export CUBE_TEMPLATE_ID=<template_id>
cd tests/e2e/sdk_compat
SDK_E2E_RUN_CODE_ENV_INHERITANCE=true \
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e \
  cases/run_code/test_python.py::test_run_code_merges_create_and_per_call_envs -q
```

`CUBE_TEMPLATE_ID` 复用 SDK 兼容性测试套件已有的模板选择机制，也可以改用 `--cube-template-id <template_id>`。环境变量继承用例默认在创建沙箱前跳过，只有确认所选模板支持该行为，并显式设置 `SDK_E2E_RUN_CODE_ENV_INHERITANCE=true` 时才会运行。
