# Python SDK

`cubesandbox` is the official Python SDK for CubeSandbox. It provides a Pythonic interface for creating sandboxes, executing code and commands, managing files, and controlling the sandbox lifecycle.

## Requirements and installation

- Python 3.9 or later

```bash
pip install cubesandbox
```

## Configuration

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote access when *.cube.app cannot be resolved
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

You can also configure the SDK in code:

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

## Quick start

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

The context manager destroys the sandbox automatically when the block exits.

## Main capabilities

- Execute code with persistent variables and streaming output.
- Run shell commands and interactive PTY sessions.
- Read, write, list, watch, rename, and remove files.
- Pause and reconnect to sandboxes while preserving memory state.
- Create snapshots, roll back, and clone sandboxes.
- Create and mount persistent volumes or host directories.
- Configure compute-node placement and L3/L4 or L7 network policies.

For the complete API reference and advanced examples, see the [Python SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/python/README.md).
