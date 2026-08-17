# Python SDK

`cubesandbox` 是 CubeSandbox 官方 Python SDK，提供符合 Python 使用习惯的接口，用于创建沙箱、执行代码与命令、管理文件以及控制沙箱完整生命周期。

## 环境要求与安装

- Python 3.9 或更高版本

```bash
pip install cubesandbox
```

## 配置

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 无法解析 *.cube.app 时，远程访问需要配置
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

也可以在代码中显式配置：

```python
from cubesandbox import Config, Sandbox

config = Config(
    api_url="http://127.0.0.1:3000",
    template_id="<your-template-id>",
    proxy_node_ip="<your-cubeproxy-node-ip>",
)

with Sandbox.create(config=config) as sandbox:
    result = sandbox.run_code("1 + 1")
    print(result.text)
```

## 快速开始

```python
from cubesandbox import Sandbox

with Sandbox.create() as sandbox:
    code = sandbox.run_code('print("hello from CubeSandbox")')
    print(code.logs.stdout)

    command = sandbox.commands.run("uname -a")
    print(command.stdout)

    sandbox.files.write("/tmp/hello.txt", "Hello, world!")
    print(sandbox.files.read("/tmp/hello.txt"))
```

退出 `with` 代码块时，上下文管理器会自动销毁沙箱。

## 主要能力

- 执行代码，支持变量持久化与流式输出。
- 执行 Shell 命令和交互式 PTY 会话。
- 读写、列出、监听、重命名和删除文件。
- 暂停并重新连接沙箱，同时保留内存状态。
- 创建快照、回滚和克隆沙箱。
- 创建并挂载持久卷或宿主机目录。
- 配置计算节点调度以及 L3/L4、L7 网络策略。

完整 API 参考和高级示例请查看 [Python SDK 中文 README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/python/README.zh.md)。
