# mini-swe-agent E2B Patch

对 mini-swe-agent 的改造代码，添加 cube-sandbox（E2B）环境支持。

## 改动文件

| 文件 | 改动说明 |
|------|---------|
| `environments/extra/e2b.py` | **新增** — E2BEnvironment 类，通过 E2B SDK 管理沙箱和执行命令 |
| `environments/__init__.py` | 在 `_ENVIRONMENT_MAPPING` 中注册 `"e2b"` 环境类型 |
| `run/benchmarks/swebench.py` | 在 `get_sb_environment()` 中将 `"e2b"` 加入支持 `image` 参数的类型列表 |

## 安装方式

安装 mini-swe-agent 后，运行 patch 脚本将改动文件覆盖到 site-packages：

```bash
pip install 'mini-swe-agent[extra]'
bash install.sh
```

脚本会自动定位 mini-swe-agent 的安装路径并覆盖对应文件。

## E2BEnvironment 核心逻辑

```
__init__()  →  Sandbox.create(template=template_id)  →  创建 E2B 沙箱
execute()   →  sbx.commands.run(cmd, user="root")     →  在沙箱中执行命令
cleanup()   →  sbx.kill()                              →  销毁沙箱
```

配置通过 YAML 的 `environment` 节传入：

```yaml
environment:
  cwd: "/testbed"
  timeout: 60
  user: "root"
  environment_class: e2b
```

`template_id` 从环境变量 `CUBE_TEMPLATE_ID` 读取。
