# Jupyter ML Sandbox

[English](README.md)

在 CubeSandbox 中运行一个基于 JupyterLab 的数据科学与 ML 工作台。
这个模板预装了 pandas、matplotlib、scikit-learn、seaborn 和 CPU 版
PyTorch，可以直接在浏览器里打开 notebook、执行 notebook，并把结果产物
保存在沙箱工作区中。

## 能展示什么

- JupyterLab 监听 `8888` 端口
- envd 仍然监听 `49983` 端口
- 使用 `nbconvert` 执行 notebook
- 一个简单的 pause/resume 状态校验
- notebook 产物保存在 `/workspace/artifacts`

## 前置条件

- Python 3.10+
- 已部署的 CubeSandbox 环境

```bash
pip install -r requirements.txt
```

## 构建镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-jupyter-ml:latest .
docker push <your-registry>/cubesandbox-jupyter-ml:latest
```

## 注册模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-jupyter-ml:latest \
  --writable-layer-size 4G \
  --expose-port 8888 \
  --expose-port 49983 \
  --probe 8888 \
  --probe-path /api/status
```

当 JupyterLab 的 `/api/status` 返回 HTTP 200 时，模板就会变成可用状态。

## 安全说明

这个示例会关闭 JupyterLab 内置的 token、password 和 XSRF 校验，让
CubeSandbox 可以通过自身的沙箱访问流程暴露 notebook，避免额外登录步骤。
notebook 服务也会以 root 运行，以匹配基础镜像和 envd 执行环境。请只在
CubeSandbox 的隔离沙箱边界内使用该配置；如果复用这个 Dockerfile 到
CubeSandbox 之外的环境，请重新开启 Jupyter 认证和 XSRF 校验。

## 配置本地运行脚本

```bash
cp .env.example .env
# 编辑 .env，填入 E2B_API_URL、E2B_API_KEY 和 CUBE_TEMPLATE_ID
```

## 运行示例

```bash
python notebook_demo.py
python pause_resume_demo.py
```

notebook demo 会打印 JupyterLab URL 和生成的产物列表。
pause/resume demo 会验证 `pause()` 之前写入的文件在 `connect()` 之后仍然存在。

## 目录结构

```
jupyter-ml-sandbox/
├── Dockerfile
├── README.md
├── README_zh.md
├── common.py
├── jupyterlab-home.png
├── jupyter_start.sh
├── notebook_demo.py
├── pause_resume_demo.py
├── requirements.txt
└── .env.example
```
