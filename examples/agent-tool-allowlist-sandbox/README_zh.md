# Agent 工具白名单沙箱

[English](README.md)

[#645](https://github.com/TencentCloud/CubeSandbox/issues/645) 的 **BYOI 受限工具箱模板**：
镜像内提供 `/usr/local/bin/cube-tool` 与可感知负载 `toolbox-hello`。

宿主脚本在 `Sandbox.create` 前拒绝非白名单 argv；推荐 `cube-tool <name>`，由
guest 再对照 `/etc/cube-sandbox/tool-profile.txt`。

**这不是：** 内核 jail、无 bash 基础镜像、语言运行时或 LLM Agent。

## 前置

- Cube 集群 + `cubemastercli`
- Docker，以及节点可拉取的镜像仓库
- Python 3.10+

```bash
cd examples/agent-tool-allowlist-sandbox
pip install -r requirements.txt
cp .env.example .env
```

## 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/agent-tool-allowlist-sandbox:latest \
  .

docker push <your-registry>/agent-tool-allowlist-sandbox:latest
```

无集群本地冒烟：

```bash
docker run --rm <your-registry>/agent-tool-allowlist-sandbox:latest \
  cube-tool toolbox-hello
# 期望 WORKLOAD_OK
```

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job_id>
```

`--probe` 指向基础镜像 **envd**。将 READY 的 template id 写入 `.env` 的
`CUBE_TEMPLATE_ID`，并配置 `E2B_API_URL`。

## 3. 配置宿主驱动

```bash
# .env
E2B_API_URL=http://<node>:3000
CUBE_TEMPLATE_ID=tpl-...
```

## 4. 运行（主路径）

```bash
python run.py
```

期望：宿主拒绝 `bash` → MicroVM → `cube-tool toolbox-hello` 输出 `WORKLOAD_OK`
→ 读到 `/workspace/out/hello.txt` → guest 拒绝 `cube-tool bash` → **`RUN_OK`**。

`verify_template.py` 是同一路径的薄别名。

## 资源建议

| 项 | 建议 |
|----|------|
| 可写层 | `--writable-layer-size 1G` |
| 端口 | expose/probe `49983`（envd） |
| CPU/内存 | 默认配额即可跑 hello 负载 |
| 扇出（extras） | 共享节点保持 `N≤2` |

## 进阶（可选）

见 [`extras/README.md`](extras/README.md)。例如：

```bash
python extras/tool_allowlist_limits.py
python extras/tool_allowlist_checkpoint.py
```

## 限制

- 基础镜像仍有 shell；绕过 `cube-tool` 不在 guest wrapper 范围内。
- 宿主白名单含裸 `cat` 时，`cat /etc/passwd` 仍过宿主门控。
- 简单重定向/通配为残差；进程替换与 `/dev/tcp`|/`/dev/udp` 会被宿主拒绝。
- 扩白名单需 `extra_binaries` + `allow_unsafe_allowlist_extension=True`。
