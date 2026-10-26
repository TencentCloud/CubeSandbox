# Cube Sandbox 完整示例

## create.py — 创建沙箱并查看信息

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    info = sandbox.get_info()
    print("sandbox info:", info)
```

## exec_code.py — 执行 Python 代码

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.run_code('print("hello cube")')
    print(result)
```

## cmd.py — 执行 Shell 命令

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)
```

## read.py — 读取沙箱内文件

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    content = sandbox.files.read("/etc/hosts")
    print(content)
```

## pause.py — 暂停与恢复

```python
import os, time
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    sandbox.pause()
    time.sleep(3)
    sandbox.connect()
    print(sandbox.get_info())
```

## create_with_mount.py — 挂载宿主机目录

```python
import os, json
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {"hostPath": "/tmp/rw", "mountPath": "/mnt/rw", "readOnly": False},
            {"hostPath": "/tmp/ro", "mountPath": "/mnt/ro", "readOnly": True},
        ])
    }
) as sandbox:
    print(sandbox.get_info())
```

## network_no_internet.py — 完全断网

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id, allow_internet_access=False) as sandbox:
    r = sandbox.commands.run("curl -s --max-time 3 https://8.8.8.8 || echo blocked")
    print(r.stdout)
```

## network_allowlist.py — 出口白名单

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={"allow_out": ["10.0.0.53/32", "10.0.1.0/24"]},
) as sandbox:
    r = sandbox.commands.run("curl -s --max-time 3 http://10.0.0.53 || echo unreachable")
    print(r.stdout)
```

## network_denylist.py — 出口黑名单

```python
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    network={"deny_out": ["10.96.0.0/16", "169.254.0.0/16"]},
) as sandbox:
    r = sandbox.commands.run("curl -s --max-time 3 https://example.com || echo blocked")
    print(r.stdout)
```
