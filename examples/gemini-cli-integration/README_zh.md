# Gemini CLI + CubeSandbox

本示例在 CubeSandbox 的硬件隔离 MicroVM 中运行 [Gemini CLI](https://github.com/google-gemini/gemini-cli)。

- `run_gemini.py`：通过宿主机环境变量注入密钥的一次性编码任务。
- `resume_gemini.py`：暂停/恢复流程，验证 `/workspace` 会随快照持久化。
- `network_policy.py`：默认拒绝出口策略和 CubeEgress `x-goog-api-key` 密钥注入，真实密钥不会进入虚拟机。

## 1. 构建并注册模板

```bash
cd examples/gemini-cli-integration
chmod +x build-template.sh
IMAGE=registry.example.com/cube/gemini-cli:2026-07-10 ./build-template.sh
```

将 `cubemastercli` 输出的模板 ID 写入 `.env`：

```bash
cp .env.example .env
# 编辑 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID、GEMINI_API_KEY
python3 -m pip install -r requirements.txt
```

## 2. 运行一次性任务

```bash
python3 run_gemini.py --approve-all
```

`--approve-all` 会显式启用 Gemini CLI 的 `--yolo` 模式。只读或需要人工确认的工作负载应省略该参数。

## 3. 验证暂停/恢复持久化

```bash
python3 resume_gemini.py --approve-all
```

脚本先让 Gemini 创建 `plan.md`，暂停 sandbox，再连接同一个 sandbox 验证文件存在，最后让 Gemini 创建 `progress.md`。

## 4. 使用凭据保险库路径

```bash
python3 network_policy.py --approve-all
```

该路径通过 `allow_internet_access=False` 创建 sandbox，只允许访问 `generativelanguage.googleapis.com`。CubeEgress 会在已允许请求上注入真实 `x-goog-api-key` 请求头，虚拟机内进程仅能获得占位密钥。

## 验证命令

```bash
python3 -m unittest test_common.py
python3 -m py_compile common.py run_gemini.py resume_gemini.py network_policy.py
bash -n build-template.sh
docker build -t gemini-cli-cube:local .
```

端到端运行还需要可用的 CubeSandbox 集群、已注册模板及 Google AI Studio API Key。不要提交 `.env`，也不要把 API Key 写入 Docker 镜像。
