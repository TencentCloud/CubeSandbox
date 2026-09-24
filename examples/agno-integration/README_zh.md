# Agno + CubeSandbox 集成示例

[English](README.md)

该示例把 Agno 的模型调用留在宿主机，只向 Agent 暴露一个自定义 Python 工具。工具不会在宿主机
启动子进程：它通过官方 `cubesandbox` SDK 将代码写入 CubeSandbox MicroVM，再由 MicroVM 中的
`python3` 执行。Agent 运行结束后，上下文管理器会清理沙箱。

## 前置条件

- 已部署 CubeSandbox；模板中应包含 `python3`，且 envd 监听 `49983`。没有现成模板时可构建本目录
  的 `Dockerfile`。
- 运行 Agent harness 的机器使用 Python 3.10+。
- 完整 Agent 流程需要一个 OpenAI 兼容 LLM 的密钥。密钥只保留在宿主机，不会被该示例传入 MicroVM。

## 运行

```bash
docker build --platform linux/amd64 -t <你的镜像仓库>/agno-cube:latest .
docker push <你的镜像仓库>/agno-cube:latest
cubemastercli tpl create-from-image \
  --image <你的镜像仓库>/agno-cube:latest \
  --writable-layer-size 1G --expose-port 49983 --probe 49983 --probe-path /health

python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
# 填写 CUBE_TEMPLATE_ID、CUBE_API_URL、CUBE_PROXY_NODE_IP 和 LLM 配置。
python agno_agent_demo.py --sandbox-only
python agno_agent_demo.py
```

`--sandbox-only` 不调用 LLM，只在 MicroVM 内运行一个确定性的 Python 计算，用来验证沙箱执行链路。
默认运行则要求 Agno Agent 调用同一个工具。

## 安全注意事项

- 应把模型生成的代码视为不可信输入。示例将单次工具输入限制为 16 KiB、命令超时限制为 120 秒，但
  这不能替代集群级别的网络与资源策略。
- 不需要出网时，保持 `allow_internet_access=False`；确有需要时，只为必要目标添加收窄的 CubeSandbox
  网络策略。
- 只有 Agent 必须跨运行保留状态时才挂载持久 Volume；否则上下文管理器的清理行为会使每次运行拥有
  独立的工作目录。
