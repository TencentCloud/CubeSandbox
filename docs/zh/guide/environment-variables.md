# 环境变量

本文介绍如何在沙箱中设置和使用环境变量，以及沙箱内的默认环境变量。

在 CubeSandbox 中设置环境变量有 3 种方法

- 创建模板时设置环境变量
- 创建沙箱时设置环境变量
- 单次命令、PTY 和代码执行时设置环境变量

同时 CubeSandbox 有多种方式与沙箱内的环境变量进行交互，**在不同时刻创建的环境变量对于不同的交互方式有不同的可见性**。

## 创建模板时

Cube 支持多种方式在创建模板时设置环境变量，可以通过在 Dockerfile 中设置环境变量，或者在 SDK 中设置环境变量，或者在 cubemastercli 中设置环境变量。

```dockerfile
ENV DEMO_ENV=template-only
```

```python
from cubesandbox import Template

job = Template.build(
    envs={"DEMO_ENV": "template-only"},
)
```

请注意，由于 envd 在启动 command/PTY 时不继承 OCI process envs，因此**在构建模板时设置的环境变量在通过 envd 处理的 sdk 命令是不可见的**。

```bash{7}
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999 \
  --env DEMO_ENV=template-only
```

此时使用 `sandbox.commands.run` 或者 `sandbox.pty.create` 命令执行时该环境变量是不可见/不生效的。

```python
from cubesandbox import Sandbox

with Sandbox.create("tpl-xxx") as sandbox:
    print(sandbox.commands.run("echo -n DEMO_ENV=$DEMO_ENV").stdout)
    # DEMO_ENV=
```

而通过 sandbox.run_code 执行的代码是可以看到该环境变量的，因为 run_code 是通过 Code Interpreter / Jupyter kernel 执行，而非 envd，因此可读取镜像 ENV 和模板构建 env。

```python
with Sandbox.create("tpl-xxx") as sandbox:
    print(sandbox.run_code("import os; print(os.environ.get('DEMO_ENV'))").logs.stdout[0].strip())
    # template-only
```

> `sandbox.commands.run()` 更常见用于执行 shell 命令、安装软件、启动服务、运行任意 CLI/编译器时。是通过 envd 处理的，因此不会继承当前创建模板时设置的环境变量。
> 
> `sandbox.run_code()` 用于执行 Python/JS/R 等代码，会继承当前创建模板时设置的环境变量。

此时设置的环境变量仅对于 run 和 pty 等 envd 处理的命令是不可见的，但是对于沙箱内的进程是可见的(包含主进程、长期服务等，run_code也算其中一种)，以及对于使用 cubecli exec 命令进入沙箱的交互式终端也是可见的。

```bash
$ cubecli exec -it <sandbox-id> bash
$ echo $DEMO_ENV
template-only
```


## 创建沙箱时

创建沙箱时可以设置环境变量，这些环境变量仅对于当前沙箱可见，不会写回模板，也不会注入容器主进程、长期服务或 `cubecli exec`。

```python
with Sandbox.create(
    template="tpl-example",
    envs={"DEMO_ENV": "sandbox-only"},
) as sandbox:
    print(sandbox.commands.run('echo -n "DEMO_ENV=$DEMO_ENV"').stdout)
    # DEMO_ENV=sandbox-only
```

> 请注意，沙箱创建时设置的这些环境变量虽然可以被 commands.run 读取，但 run_code 默认无法读取，因为其 lightweight code interpreter 不会将 envd 中的沙箱环境变量注入 Jupyter kernel 中，若希望 run_code() 自动继承 Sandbox.create(envs=...)，请参考使用仓库的 run-code-env-inheritance 派生镜像： [run-code-env-inheritance](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README_zh.md)。

## 单次命令、PTY 和代码执行时

调用级 `envs` 仅作用于当前调用，并覆盖创建 Sandbox 时的同名变量：

```python
with Sandbox.create(
    template="tpl-example",
    envs={"DEMO_ENV": "sandbox-only"},
) as sandbox:
    print(sandbox.commands.run('echo -n "DEMO_ENV=$DEMO_ENV"',
    envs={"DEMO_ENV": "one-shot"}).stdout)
    # DEMO_ENV=one-shot
```

此时创建的这些环境变量仅对当前的调用可见。

## 环境变量的可见性总结

| 来源 | commands.run / SDK PTY | cubecli exec / OCI Exec | 主进程 / 服务 |
|---|---|---|---|
| 镜像 ENV、模板 build envs | 不应依赖 / 当前通常不可见 | 可见 | 可见 |
| Sandbox.create(envs=...) | 可见 | 不可见 | 不可见 |
| command / PTY 调用 envs | 当前调用可见，优先级最高 | 不适用 | 不影响 |
| run_code(..., envs=...) | 不适用 | 仅当前执行 | 不适用 |