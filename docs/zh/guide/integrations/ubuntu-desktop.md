---
title: Ubuntu 桌面沙箱指南
author: fslongjin
date: 2026-09-15
tags:
  - integration
  - ubuntu-desktop
  - novnc
  - gui-automation
lang: zh-CN
---

# Ubuntu 桌面沙箱

在 Cube 沙箱里跑一个完整的 Ubuntu 22.04 GNOME 桌面，用浏览器打开即可操作。适用于 GUI 自动化、截图测试、Agent 驱动的桌面任务，以及任何需要真实显示器的场景。

## 你会得到什么

- Ubuntu 22.04，默认 Yaru 主题和左侧 Dock（文件管理器、终端）
- 1280×720 的桌面画面，通过 noVNC 在 `6080` 端口输出
- 键鼠、剪贴板、截图均可用
- 可通过 SDK 脚本化：带上 `DISPLAY=:0` 运行命令即可操作 GUI

## 前置条件

- 一个可用的 Cube 集群，且能访问 `cubemastercli`
- 本机已安装 Docker，用于本地测试
- `cubesandbox` Python SDK，用于程序化访问

## 快速开始

### 1. 构建并推送镜像

```bash
cd examples/ubuntu-desktop
docker build -t <registry>/ubuntu-desktop:v1 .
docker push <registry>/ubuntu-desktop:v1
```

### 2. 创建模板

```bash
cubemastercli tpl create-from-image \
  --image <registry>/ubuntu-desktop:v1 \
  --alias ubuntu-desktop \
  --writable-layer-size 16Gi \
  --cpu 4000 \
  --memory 8192 \
  --expose-port 6080 \
  --expose-port 49983 \
  --probe 6080 \
  --probe-path / \
  --allow-internet-access
```

探针指向 `6080`（noVNC），确保桌面真正起来后才标记沙箱就绪。

### 3. 打开桌面

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template="ubuntu-desktop", timeout=-1)
print("桌面:", f"https://{sb.get_host(6080)}/")
```

浏览器打开打印的地址，就能看到 Ubuntu 桌面。

## 程序操作桌面

沙箱里的桌面跑在 `DISPLAY=:0`，运行命令时带上这个变量：

```python
# 截图
sb.commands.run("DISPLAY=:0 gnome-screenshot -f /tmp/desk.png")
print(sb.files.read("/tmp/desk.png"))

# 列出当前窗口
sb.commands.run("DISPLAY=:0 wmctrl -l")
```

## 访问方式

CubeProxy 提供两种方式打开桌面：

- **Host 模式**（默认）：`https://6080-<sandbox-id>.cube.app/` — 与 noVNC 默认的 WebSocket 路径兼容，推荐使用。
- **Path 模式**：`https://<proxy>/sandbox/<sandbox-id>/6080/` — 不需要泛域名 DNS，但 noVNC 的 WebSocket 需要带完整路径。可以通过 query 参数 `path=sandbox/<id>/6080/websockify` 传入，或用一个小型反代设置 `Host` 头。

## 本地测试

推送前，先在本机验证镜像：

```bash
cd examples/ubuntu-desktop
./run-local.sh
# 打开 http://127.0.0.1:6080/
```

## 参考

- 示例仓库：[examples/ubuntu-desktop](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/ubuntu-desktop)
- noVNC：[https://novnc.com](https://novnc.com)
