# Agent 工具白名单（BYOI）

基于 [Bring Your Own Image](../../docs/guide/tutorials/bring-your-own-image.md)
的小模板，对应 [#645](https://github.com/TencentCloud/CubeSandbox/issues/645)：
可构建镜像，内含 `/etc/cube-sandbox/tool-profile.txt`，与
[`../code-sandbox-quickstart/tool_allowlist.py`](../code-sandbox-quickstart/tool_allowlist.py)
的宿主机 argv 门控对齐。

和常见 Agent 沙箱用法一致（E2B 宿主、OpenAI Agents 的 `E2BSandboxClient`
等）：**策略在宿主机，负载在沙箱**。Cube 侧再叠 MicroVM 与
`allow_internet_access=False`。

本目录负责模板与冒烟；门控单测与参考调度环放在 `code-sandbox-quickstart/`，
避免门控绑死某一张镜像。

## 构建

```bash
docker build -t agent-tool-allowlist-sandbox:latest .

# 可选：需要 guest 内 curl 做出口探测时再打开
# docker build --build-arg INSTALL_CURL=1 -t agent-tool-allowlist-sandbox:latest .
```

本地探活（与 `cubesandbox-base-nginx` 同类）：

```bash
docker run --rm -d --name agent-tool-box \
  -p 49983:49983 agent-tool-allowlist-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f agent-tool-box
```

## 注册模板

```bash
cubemastercli tpl create-from-image \
  --image <仓库或本地>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

READY 后把 template id 写入 `.env` 的 `CUBE_TEMPLATE_ID`。

资源：可写层 1G 足够；不需要 GPU。

## 配置与运行

```bash
pip install -r requirements.txt
cp .env.example .env   # E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID

python verify_template.py
```

预期：宿主机拒绝 `bash`，再读 `tool-profile` 与 `echo`，输出 `TEMPLATE_VERIFY_OK`。

只测门控（不必构建模板）：

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

模板注册后：

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_allow.py
python tool_agent_loop.py
```

`tool_agent_loop.py` 是写死的 propose 列表，不是 LLM。出口探测需要 guest
里有 `curl`（基础镜像或 `INSTALL_CURL=1`）；否则会跳过该轮。

## 限制

- 基础镜像仍有 shell；profile 只表示意图，不是 guest 隔离本身。
- 默认白名单下 `echo … > file` 仍可写 guest 路径——见 quickstart 威胁模型。
- 不能替代 `sandbox-code` / 解释器类模板。
- `create-from-image` 需要节点能拉取的镜像引用（推到集群可达仓库；裸
  `*:local` 会走 Docker Hub 解析）。
- 默认构建不会 apt 安装 curl。基础镜像里可能已有 curl，那是继承，不属于
  `tool-profile.txt`。只有要钉死进自己层时才用 `INSTALL_CURL=1`。

[English](README.md)

[English](README.md)
