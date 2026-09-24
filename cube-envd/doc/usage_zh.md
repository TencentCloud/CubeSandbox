# 创建模板并使用 cube-envd

[English](usage.md) · [组件概览](../README_zh.md)

以下命令均从仓库根目录执行。先按 [README](../README_zh.md#构建沙箱镜像) 构建镜像。

先为已有部署配置 `cubemastercli`，再从构建器可读取的镜像创建模板。若构建器
使用 Docker exporter 且能够读取构建镜像时的同一 Docker 镜像存储，可执行下列
命令。默认 native exporter 从 registry 拉取镜像，不会读取本地 Docker tag；
本地 Docker 导出需在构建服务中配置 `CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED=false`，
且没有优先使用的 skopeo/umoci。具体条件见
[选定 envd 验收](../../tests/e2e/sdk_compat/README_zh.md#选定-envd-验收)。
使用默认 exporter 时，请先通过已有镜像分发方式提供 registry 镜像引用，并替换
下列 `--image` 值。

```bash
cubemastercli tpl create-from-image \
  --image cubesandbox-demo-nginx:rust-local \
  --cpu 1000 --memory 512 \
  --writable-layer-size 1G \
  --expose-port 49983 --expose-port 80 \
  --probe 49983 --probe-path /health
```

其他情况请使用平台现有镜像分发方式能够访问的镜像引用。CLI 配置与镜像访问
方式见[自定义模板镜像教程](../../docs/zh/guide/tutorials/bring-your-own-image.md)。
CLI 默认跟踪构建进度；应等待构建成功且模板达到 **READY**。就绪探针检查的是
envd 的 `49983/health`，不是 nginx 的 80 端口或 Jupyter 接口。保留生成的模板 ID，
供 SDK 使用。

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e ./sdk/python

# 根据已有部署和刚创建的模板修改这些值。
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-replace-with-your-template
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_PROXY_PORT_HTTP=80
export CUBE_SANDBOX_DOMAIN=cube.app
export NO_PROXY=localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
```

`CUBE_PROXY_NODE_IP` 填 CubeProxy 节点，不能填写 guest IP；若部署的 sandbox DNS
已能正确路由，可以省略。平台启用认证时应提供 `CUBE_API_KEY`；HTTPS 使用私有
CA 时，为 SDK 的 HTTP 客户端配置 `SSL_CERT_FILE` 和 `REQUESTS_CA_BUNDLE`。

```python
import os
from cubesandbox import Sandbox, Template

template_id = os.environ["CUBE_TEMPLATE_ID"]
assert Template.get(template_id).status == "READY"
sandbox = Sandbox.create(timeout=300)
try:
    result = sandbox.commands.run("printf 'hello from cube-envd'", timeout=30)
    assert result.exit_code == 0
    assert result.stdout == "hello from cube-envd"
    assert result.stderr == ""

    content = "hello from the SDK\n你好\n"
    sandbox.files.write("/tmp/envd-example.txt", content)
    assert sandbox.files.read("/tmp/envd-example.txt") == content
    print(sandbox.sandbox_id, result.stdout)
finally:
    sandbox.kill()
```

示例会删除本次 sandbox，保留模板供重复使用。所有相关 sandbox 删除后，如果不再
需要该模板，可使用 `Template.delete(template_id)` 删除它。若要运行完整的镜像、
模板、sandbox 流程，并自动检查 health 204、运行中 daemon 身份、命令与文件行为、
记录日志并清理，请使用已有的
[选定 envd 验收入口](../../tests/e2e/sdk_compat/README_zh.md#选定-envd-验收)。
该入口也提供三个可独立执行的场景。
